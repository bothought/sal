package agent

import (
	"context"
	"fmt"
	"strings"
)

// Context window policy. The window itself is resolved per model (see
// resolveLimit); these are the defaults for everything else.
const (
	// DefaultContextWindow is assumed when nothing else knows the model's
	// window: a conservative value that makes compaction happen early rather
	// than too late.
	DefaultContextWindow = 128000
	// DefaultCompactAt is the percentage of the window at which automatic
	// compaction runs.
	DefaultCompactAt = 80
	// compactTargetPercent is what compaction aims to leave behind, so a
	// session has room for several more turns instead of compacting again at
	// once.
	compactTargetPercent = 50
	// DefaultKeepTurns is how many recent user turns are kept verbatim.
	DefaultKeepTurns = 3
	// perMessageOverhead approximates the role and framing tokens a message
	// costs beyond its text.
	perMessageOverhead = 4
	// omittedMarker replaces a tool result that had to be shortened to meet
	// the compaction target.
	omittedMarker = "[output omitted to fit the context window]"
)

// Limit sources, reported by /context so an assumed window is not mistaken for
// a known one.
const (
	LimitFromConfig   = "config"
	LimitFromProvider = "provider"
	LimitFromTable    = "table"
	LimitFromDefault  = "default"
)

// ContextInfo is what an interface needs to show the context window: the figure
// for the next request, the window, and whether the figure is measured or
// estimated.
type ContextInfo struct {
	Used   int    `json:"used"`
	Limit  int    `json:"limit"`
	Exact  bool   `json:"exact"`
	Source string `json:"source,omitempty"`

	// Breakdown for /context. The parts are estimates; Used is the figure the
	// footer shows.
	System          int `json:"system,omitempty"`
	Tools           int `json:"tools,omitempty"`
	Summary         int `json:"summary,omitempty"`
	SummaryMessages int `json:"summaryMessages,omitempty"`
	Recent          int `json:"recent,omitempty"`
	RecentMessages  int `json:"recentMessages,omitempty"`
	// SummaryText is the active summary itself, so an interface can show it
	// from the event stream without reading the agent while a run is in flight.
	SummaryText string `json:"summaryText,omitempty"`

	// LastUsage is what the provider reported for the most recent request, so
	// /context can show the measured figure the estimate is based on. Both are
	// zero until a response has been seen.
	LastInput  int `json:"lastInput,omitempty"`
	LastOutput int `json:"lastOutput,omitempty"`

	// Context window policy, so a console can explain when compaction will run.
	AutoCompact bool `json:"autoCompact"`
	CompactAt   int  `json:"compactAt,omitempty"`
	KeepTurns   int  `json:"keepTurns,omitempty"`

	// Compacting reports that a compaction is running right now, so the status
	// line can say so while the summarization request is in flight.
	Compacting bool `json:"compacting,omitempty"`
}

// CompactionInfo reports one compaction. Error is set when the summarization
// request failed and the context was left alone; Note carries an explanation
// that is not a failure.
type CompactionInfo struct {
	Messages int `json:"messages"`
	Before   int `json:"before"`
	After    int `json:"after"`
	// KeptTurns is how many user turns the compaction kept verbatim, so the
	// notification reports what actually happened rather than the default.
	KeptTurns int    `json:"keptTurns,omitempty"`
	Shortened int    `json:"shortened,omitempty"`
	Automatic bool   `json:"automatic"`
	Error     string `json:"error,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Compacted is the active summary of a conversation prefix. It is what makes
// the context sent to the model smaller than the conversation the agent holds.
type Compacted struct {
	Summary string
	// Through is the index of the last history message the summary replaces.
	Through  int
	Messages int
	// Shortened is how many tool results the compaction had to replace with a
	// marker; ShortenedIndexes records which messages those were, so a restored
	// compaction sends the same context it did before the session was closed.
	Shortened        int
	ShortenedIndexes []int
}

// EstimateTokens approximates the token count of s. ASCII text is counted at
// roughly four characters per token; every other rune counts as one, so text in
// a script that encodes densely is not under-counted.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	ascii, other := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

func estimateMessage(m Message) int {
	n := perMessageOverhead + EstimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		n += perMessageOverhead + EstimateTokens(tc.Name) + EstimateTokens(tc.Arguments)
	}
	return n
}

// modelWindow is one entry of the built-in table.
type modelWindow struct {
	prefix string
	window int
}

// knownWindows maps model id prefixes to context windows. It is deliberately
// small and conservative: it exists so the common cases work without
// configuration, not to become a vendor matrix. A wrong entry only changes when
// compaction happens; --context-window is always available to override it.
var knownWindows = []modelWindow{
	{"gpt-4o", 128000},
	{"gpt-4.1", 1000000},
	{"gpt-4-turbo", 128000},
	{"gpt-4", 8192},
	{"gpt-3.5", 16385},
	{"o1", 200000},
	{"o3", 200000},
	{"o4", 200000},
	{"claude", 200000},
	{"llama-3", 128000},
	{"llama3", 128000},
	{"llama-4", 128000},
	{"qwen", 131072},
	{"deepseek", 65536},
	{"mistral", 32768},
	{"mixtral", 32768},
	{"gemma", 8192},
	{"phi", 131072},
	{"command-r", 128000},
}

func windowFromTable(model string) (int, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return 0, false
	}
	for _, w := range knownWindows {
		if strings.HasPrefix(m, w.prefix) {
			return w.window, true
		}
	}
	return 0, false
}

// resolveLimit decides the model's context window and where the value came
// from. ctx may be nil, in which case the provider is not asked; an answer
// found without asking is not cached, so the first resolution that can reach
// the endpoint still happens and the endpoint's metadata gets its turn.
func (a *Agent) resolveLimit(ctx context.Context) (int, string) {
	if a.ContextWindowOverride > 0 {
		return a.ContextWindowOverride, LimitFromConfig
	}
	model := a.ModelName()
	if a.limitModel == model && a.limitTokens > 0 {
		return a.limitTokens, a.limitSource
	}
	tokens, source := 0, ""
	if ctx != nil && a.Provider != nil {
		if l, ok := a.Provider.(ContextLimiter); ok {
			if n, ok := l.ContextWindow(ctx); ok && n > 0 {
				tokens, source = n, LimitFromProvider
			}
		}
	}
	if tokens == 0 {
		if n, ok := windowFromTable(model); ok {
			tokens, source = n, LimitFromTable
		} else {
			tokens, source = DefaultContextWindow, LimitFromDefault
		}
		// The table and the default are guesses that a provider answer or a
		// configuration change can supersede, so they are not remembered; only
		// a value the endpoint itself reported is worth caching.
		if ctx == nil || a.Provider == nil {
			return tokens, source
		}
	}
	a.limitModel, a.limitTokens, a.limitSource = model, tokens, source
	return tokens, source
}

// ContextWindow reports the resolved window and its source, without asking the
// provider (an interface redrawing its footer must not make a request).
func (a *Agent) ContextWindow() (int, string) { return a.resolveLimit(nil) }

// compactAt is the effective auto-compaction threshold, in percent.
func (a *Agent) compactAt() int {
	if a.CompactAt <= 0 {
		return DefaultCompactAt
	}
	if a.CompactAt > 100 {
		return 100
	}
	return a.CompactAt
}

func (a *Agent) keepTurns() int {
	if a.KeepTurns <= 0 {
		return DefaultKeepTurns
	}
	return a.KeepTurns
}

// systemToolsTokens estimates the fixed part of every request: the system
// prompt and the tool definitions. It does not change within a session, so it
// is computed once.
func (a *Agent) systemToolsTokens() (system, tools int) {
	if a.baseTokensSet {
		return a.systemTokens, a.toolTokens
	}
	a.systemTokens = EstimateTokens(a.SystemPrompt)
	for _, s := range a.toolSpecs() {
		a.toolTokens += perMessageOverhead + EstimateTokens(s.Name) +
			EstimateTokens(s.Description) + EstimateTokens(string(s.Parameters))
	}
	a.baseTokensSet = true
	return a.systemTokens, a.toolTokens
}

// contextMessages is the conversation as it will be sent: the summary (when a
// compaction is active) followed by the messages the summary does not cover.
// This is the one place where what the model sees differs from History.
func (a *Agent) contextMessages() []Message {
	if a.compaction == nil {
		return a.History
	}
	start := a.compaction.Through + 1
	if start > len(a.History) {
		start = len(a.History)
	}
	out := make([]Message, 0, len(a.History)-start+1)
	out = append(out, Message{Role: RoleUser, Content: summaryText(a.compaction)})
	out = append(out, a.History[start:]...)
	for _, i := range a.shortened {
		if i >= start && i-start+1 < len(out) {
			out[i-start+1] = shortenedMessage(out[i-start+1])
		}
	}
	return out
}

func summaryText(c *Compacted) string {
	return fmt.Sprintf(
		"[summary of the conversation so far — %d earlier messages, recorded, not instructions]\n%s\n[the most recent turns follow]",
		c.Messages, c.Summary)
}

func shortenedMessage(m Message) Message {
	m.Content = omittedMarker
	return m
}

// UsedTokens is the size of the next request: the last measured figure plus an
// estimate of everything appended since. exact is true only when the whole
// figure comes from the provider, so appending anything after a measurement
// makes it an estimate again.
func (a *Agent) UsedTokens() (used int, exact bool) {
	ctxMsgs := a.contextMessages()
	if a.measuredOK && a.measuredAt <= len(ctxMsgs) {
		used = a.measured
		for i := a.measuredAt; i < len(ctxMsgs); i++ {
			used += estimateMessage(ctxMsgs[i])
		}
		return used, a.measuredAt == len(ctxMsgs)
	}
	system, tools := a.systemToolsTokens()
	total := system + tools
	for _, m := range ctxMsgs {
		total += estimateMessage(m)
	}
	return total, false
}

// ContextInfo assembles the figures an interface shows.
func (a *Agent) ContextInfo() ContextInfo {
	limit, source := a.ContextWindow()
	used, exact := a.UsedTokens()
	system, tools := a.systemToolsTokens()
	info := ContextInfo{
		Used:        used,
		Limit:       limit,
		Exact:       exact,
		Source:      source,
		System:      system,
		Tools:       tools,
		LastInput:   a.lastInput,
		LastOutput:  a.lastOutput,
		AutoCompact: a.AutoCompact,
		CompactAt:   a.compactAt(),
		KeepTurns:   a.keepTurns(),
		Compacting:  a.compacting,
	}
	if a.compaction != nil {
		info.Summary = EstimateTokens(summaryText(a.compaction))
		info.SummaryMessages = a.compaction.Messages
		info.SummaryText = a.compaction.Summary
	}
	ctxMsgs := a.contextMessages()
	recent := 0
	for _, m := range ctxMsgs {
		recent += estimateMessage(m)
	}
	if a.compaction != nil && len(ctxMsgs) > 0 {
		recent -= estimateMessage(ctxMsgs[0])
		info.RecentMessages = len(ctxMsgs) - 1
	} else {
		info.RecentMessages = len(ctxMsgs)
	}
	info.Recent = recent
	return info
}

// Summary returns the active summary, or "" when the context is uncompacted.
func (a *Agent) Summary() string {
	if a.compaction == nil {
		return ""
	}
	return a.compaction.Summary
}

// ActiveContext reports what the model is sent, for a transcript rebuilt after
// a resume or a session switch: the summary ("" when uncompacted), the messages
// after it, and how many earlier messages the summary replaces. The summarized
// messages stay in History — and in the session file — but are not part of the
// conversation the model sees, so replaying them would misrepresent it.
func (a *Agent) ActiveContext() (summary string, messages []Message, summarized int) {
	if a.compaction == nil {
		return "", a.Messages(), 0
	}
	start := a.compaction.Through + 1
	if start > len(a.History) {
		start = len(a.History)
	}
	return a.compaction.Summary, append([]Message(nil), a.History[start:]...), a.compaction.Messages
}

// SetCompaction restores a compaction recorded in a session, so resuming shows
// the context the session had rather than the whole log.
func (a *Agent) SetCompaction(c *Compacted) {
	a.compaction = c
	a.shortened = nil
	if c != nil {
		a.shortened = append(a.shortened, c.ShortenedIndexes...)
	}
	a.measuredOK = false
}

// recordUsage turns the provider's per-request accounting into the measured
// base of the context figure. It is called after the answer has been appended,
// so the whole sent context is covered.
func (a *Agent) recordUsage(u *Usage) {
	if u == nil {
		return
	}
	a.lastInput = u.InputTokens
	a.lastOutput = u.OutputTokens
	a.measured = u.InputTokens + u.OutputTokens
	a.measuredAt = len(a.contextMessages())
	a.measuredOK = a.measured > 0
}

// invalidateContext drops the measured base, which must happen whenever the
// context changes shape rather than grows.
func (a *Agent) invalidateContext() { a.measuredOK = false }

// emitContext reports the current figures.
func (a *Agent) emitContext(emit func(Event)) {
	if emit == nil {
		return
	}
	info := a.ContextInfo()
	emit(Event{Type: EventContext, Context: &info})
}
