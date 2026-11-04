// Package agent implements sal's core agent loop: it turns a user prompt into
// one or more provider requests, executes the tool calls the model asks for,
// and appends every message to the conversation history.
//
// The loop builds a request from system prompt + history + tools, streams the
// assistant response, executes each tool call, records the results, and repeats
// until the model answers without requesting tools.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// Conversation roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Event types emitted during a run.
const (
	EventTextDelta  = "text_delta"
	EventToolStart  = "tool_start"
	EventToolResult = "tool_result"
	EventTurnEnd    = "turn_end"
	EventUsage      = "usage"
	EventDone       = "done"
	EventError      = "error"
	// EventRetry reports that a transient provider failure is being retried.
	EventRetry = "retry"
	// EventContext reports the size of the next request against the model's
	// context window.
	EventContext = "context"
	// EventCompaction reports that older messages were replaced by a summary.
	EventCompaction = "compaction"
)

// MaxToolResultChars caps a single tool result before it is fed back to the
// model, so one huge command output cannot blow up the context window.
const MaxToolResultChars = 30000

// ToolCall is a single function invocation requested by the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one entry of the conversation in provider-neutral form.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	ToolName   string     `json:"toolName,omitempty"`
	// Usage is only set on assistant messages returned by a provider.
	Usage     *Usage `json:"usage,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Usage reports token accounting for one provider request.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

// Tool is implemented by every built-in tool.
type Tool interface {
	Name() string
	Description() string
	// Parameters returns a JSON Schema object for the tool arguments.
	Parameters() json.RawMessage
	// Run executes the tool. A returned error is reported to the model as a
	// failed tool result rather than aborting the run.
	Run(ctx context.Context, args json.RawMessage) (string, error)
}

// Provider streams model responses. The bundled implementation is
// OpenAI-compatible (see internal/provider).
type Provider interface {
	Name() string
	ModelName() string
	Stream(ctx context.Context, messages []Message, tools []ToolSpec, onText func(string)) (Message, error)
}

// ContextLimiter is implemented by providers that can report the model's
// context window. Providers that cannot are asked nothing; the window then
// comes from the built-in table or the default.
type ContextLimiter interface {
	// ContextWindow reports the model's window in tokens, or false when the
	// endpoint does not publish one. A failure to find out is not an error.
	ContextWindow(ctx context.Context) (int, bool)
}

// ModelSetter is implemented by providers that let the model be switched at
// runtime (used by the TUI's /model command).
type ModelSetter interface{ SetModelName(string) }

// SessionIDSetter is implemented by providers that accept a session affinity
// id, used for routing and prompt-cache affinity on gateways that want one.
type SessionIDSetter interface{ SetSessionID(string) }

// ToolResult is the outcome of one tool execution.
type ToolResult struct {
	Name       string `json:"name"`
	Content    string `json:"content"`
	IsError    bool   `json:"isError"`
	DurationMS int64  `json:"durationMs"`
}

// Event is what the agent reports while running. The TUI and print/JSON modes
// subscribe to these.
type Event struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ToolCall *ToolCall   `json:"toolCall,omitempty"`
	Result   *ToolResult `json:"result,omitempty"`
	Message  *Message    `json:"message,omitempty"`
	Usage    *Usage      `json:"usage,omitempty"`
	Error    string      `json:"error,omitempty"`
	// Retry fields describe a transient failure being retried (EventRetry).
	Attempt     int `json:"attempt,omitempty"`
	MaxAttempts int `json:"maxAttempts,omitempty"`
	RetryInMS   int `json:"retryInMs,omitempty"`
	// Context describes the context window (EventContext).
	Context *ContextInfo `json:"context,omitempty"`
	// Compaction describes one compaction (EventCompaction).
	Compaction *CompactionInfo `json:"compaction,omitempty"`
}

// Agent drives the loop.
type Agent struct {
	Provider     Provider
	Tools        []Tool
	SystemPrompt string
	History      []Message
	MaxTurns     int
	// MaxRetries is how many times a transient provider failure (a reset
	// connection, a 429, a 5xx) is retried before the turn fails. Zero means
	// DefaultMaxRetries; a negative value disables retrying.
	MaxRetries int
	// RetryBaseDelay is the first backoff step; it doubles per attempt. Zero
	// means DefaultRetryBaseDelay.
	RetryBaseDelay time.Duration
	// ContextWindowOverride, when positive, is the model's context window in
	// tokens, overriding discovery. Zero means resolve it (see resolveLimit).
	ContextWindowOverride int
	// AutoCompact enables compaction of older messages when the context
	// approaches ContextWindow. The zero value is off; New sets it on.
	AutoCompact bool
	// CompactAt is the percentage of the window at which automatic compaction
	// runs. Zero means DefaultCompactAt.
	CompactAt int
	// KeepTurns is how many recent user turns are kept verbatim. Zero means
	// DefaultKeepTurns.
	KeepTurns int
	// Sleep waits between retries. It is a field so tests do not wait.
	Sleep func(context.Context, time.Duration) error
	// OnEvent receives every event; never nil after New.
	OnEvent func(Event)
	// OnMessage receives every message appended to the history, which is how
	// sessions persist themselves.
	OnMessage func(Message)
	// OnCompaction receives every compaction, so a session can record it and
	// resume from the summary rather than the whole log.
	OnCompaction func(Compacted)

	// Resolved context window and its source, cached per model.
	limitModel  string
	limitTokens int
	limitSource string
	// Fixed part of every request (system prompt, tool definitions), cached.
	baseTokensSet bool
	systemTokens  int
	toolTokens    int
	// Last measured context size and the message index it covers, plus what the
	// provider reported for that request.
	measured   int
	measuredAt int
	measuredOK bool
	lastInput  int
	lastOutput int
	// compaction is the active summary, or nil when the context is complete.
	compaction *Compacted
	// shortened lists history indexes whose tool result is sent as a marker.
	shortened []int
	// autoDisabled keeps the threshold check from retrying a compaction that
	// cannot free enough of the window, for the rest of the session.
	autoDisabled bool
	// compacting is true while an automatic compaction is in flight, so the
	// status line can say so.
	compacting bool
}

// Retry policy defaults for transient provider failures.
const (
	DefaultMaxRetries     = 4
	DefaultRetryBaseDelay = 500 * time.Millisecond
	maxRetryDelay         = 10 * time.Second
)

// RetryableError marks a provider failure that is worth retrying, with an
// optional delay the server asked for (Retry-After). Providers wrap transient
// failures (a reset connection, a 429, a 5xx) in it; the agent loop decides how
// many times to retry and how long to wait.
type RetryableError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// New creates an agent computing a default system prompt for the working
// directory.
func New(p Provider, t []Tool, cwd string, extraPrompt string) *Agent {
	sys := BuildSystemPrompt(cwd, extraPrompt)
	a := &Agent{
		Provider:       p,
		Tools:          t,
		SystemPrompt:   sys,
		MaxTurns:       32,
		MaxRetries:     DefaultMaxRetries,
		RetryBaseDelay: DefaultRetryBaseDelay,
		AutoCompact:    true,
		CompactAt:      DefaultCompactAt,
		KeepTurns:      DefaultKeepTurns,
		Sleep:          sleepCtx,
		OnEvent:        func(Event) {},
	}
	if p != nil {
		a.Provider = p
	}
	return a
}

// sleepCtx waits for d, or returns early when the context is cancelled, so an
// abort during a retry backoff stops the turn promptly.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func nowMilli() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// NewUserMessage builds a user message stamped with the current time.
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Content: text, Timestamp: nowMilli()}
}

// ModelName reports the active model.
func (a *Agent) ModelName() string {
	if a.Provider == nil {
		return ""
	}
	return a.Provider.ModelName()
}

// SetModelName switches the model when the provider supports it.
func (a *Agent) SetModelName(name string) bool {
	if s, ok := a.Provider.(ModelSetter); ok && name != "" {
		s.SetModelName(name)
		// The window belongs to the model, so the cached one no longer applies.
		a.limitModel = ""
		a.limitTokens = 0
		a.limitSource = ""
		a.measuredOK = false
		return true
	}
	return false
}

// SetSessionID forwards the conversation id to the provider when it supports
// one, so the gateway can keep routing and cache affinity stable.
func (a *Agent) SetSessionID(id string) bool {
	if s, ok := a.Provider.(SessionIDSetter); ok && id != "" {
		s.SetSessionID(id)
		return true
	}
	return false
}

// ProviderName reports the active provider.
func (a *Agent) ProviderName() string {
	if a.Provider == nil {
		return ""
	}
	return a.Provider.Name()
}

// Messages returns a copy of the conversation history.
func (a *Agent) Messages() []Message {
	out := make([]Message, len(a.History))
	copy(out, a.History)
	return out
}

// Reset clears the conversation history (TUI /new).
func (a *Agent) Reset() {
	a.History = nil
	a.compaction = nil
	a.shortened = nil
	a.measuredOK = false
	a.autoDisabled = false
}

// SetHistory replaces the conversation, dropping any compaction and the
// measured context figure: both describe a conversation that is being left.
func (a *Agent) SetHistory(msgs []Message) {
	a.History = append([]Message(nil), msgs...)
	a.compaction = nil
	a.shortened = nil
	a.measuredOK = false
	a.autoDisabled = false
}

// compactForContext runs an automatic compaction when the context has reached
// the configured threshold. It runs at most once per turn and disables itself
// for the session when compacting does not bring the context below the
// threshold, so a run can never stall trying to shrink itself.
func (a *Agent) compactForContext(ctx context.Context, emit func(Event)) {
	if !a.AutoCompact || a.autoDisabled {
		return
	}
	limit, _ := a.resolveLimit(ctx)
	if limit <= 0 {
		return
	}
	used, _ := a.UsedTokens()
	if used*100 < limit*a.compactAt() {
		return
	}
	// Tell the interface compaction is running now, so the status shows it for
	// the duration of the summarization request.
	a.compacting = true
	a.emitContext(emit)
	info, err := a.Compact(ctx, "", true)
	a.compacting = false
	if err != nil {
		// A failed automatic compaction must not block the run: the request
		// proceeds with the context as it is, and its own outcome is what the
		// user sees.
		switch {
		case errors.Is(err, ErrNothingToCompact):
			// Only the kept tail is left, so there is nothing to do and no
			// reason to try again this session.
			a.autoDisabled = true
			a.emitContext(emit)
			return
		case ctx.Err() != nil:
			// An abort during the summarization is not a failure of the
			// context: the transcript says the compaction was cancelled, since
			// the run's own failure notice is suppressed for an abort.
			info.Note = "compaction cancelled, the context is unchanged"
			emit(Event{Type: EventCompaction, Compaction: &info})
			a.emitContext(emit)
			return
		}
		info.Error = err.Error()
		emit(Event{Type: EventCompaction, Compaction: &info})
		a.emitContext(emit)
		return
	}
	if after, _ := a.UsedTokens(); after*100 >= limit*a.compactAt() {
		// Still full: the fixed part is large or the summarizable part is tiny.
		// Sending and failing once beats summarizing repeatedly for no gain.
		a.autoDisabled = true
		info.Note = "automatic compaction is now off for this session: compacting did not free enough of the window"
	}
	emit(Event{Type: EventCompaction, Compaction: &info})
	a.emitContext(emit)
}

// ToolNames lists the enabled tools in registration order.
func (a *Agent) ToolNames() []string {
	names := make([]string, 0, len(a.Tools))
	for _, t := range a.Tools {
		names = append(names, t.Name())
	}
	return names
}

// RunPrompt runs one user prompt through the agent loop, emitting events as it
// goes. It returns when the model stops requesting tools, or on error.
func (a *Agent) RunPrompt(ctx context.Context, prompt string, emit func(Event)) error {
	if emit == nil {
		emit = a.OnEvent
	}
	if emit == nil {
		emit = func(Event) {}
	}
	if prompt != "" {
		msg := NewUserMessage(prompt)
		a.append(msg, emit)
	}
	// Resolve the window now, while a context that can reach the endpoint is in
	// hand, and only then report it. Asking here rather than leaving it to the
	// compaction check is what makes the first figure the model's real one: the
	// check returns early when auto-compaction is off, so a run with it disabled
	// would otherwise report the table's guess for the whole conversation.
	a.resolveLimit(ctx)
	a.emitContext(emit)
	if a.Provider == nil {
		err := fmt.Errorf("no model provider configured: set SAL_API_KEY or OPENAI_API_KEY")
		emit(Event{Type: EventError, Error: err.Error()})
		return err
	}

	maxTurns := a.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 32
	}

	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Before the request goes out, check whether the context has filled the
		// window and compact if so. The check runs once per turn and never
		// loops (see compactForContext).
		a.compactForContext(ctx, emit)

		var streamed strings.Builder
		assistant, err := a.streamWithRetry(ctx, emit, func(delta string) {
			streamed.WriteString(delta)
			emit(Event{Type: EventTextDelta, Text: delta})
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			emit(Event{Type: EventError, Error: err.Error()})
			return err
		}
		if assistant.Content == "" {
			assistant.Content = streamed.String()
		}
		assistant.Role = RoleAssistant
		if assistant.Timestamp == "" {
			assistant.Timestamp = nowMilli()
		}
		if assistant.Usage != nil {
			emit(Event{Type: EventUsage, Usage: assistant.Usage})
		}
		a.append(assistant, emit)
		// The provider has just reported the exact size of what it was sent, so
		// the figure is measured until the next message is appended.
		a.recordUsage(assistant.Usage)
		a.emitContext(emit)

		if len(assistant.ToolCalls) == 0 {
			emit(Event{Type: EventDone})
			return nil
		}

		for i := range assistant.ToolCalls {
			call := assistant.ToolCalls[i]
			emit(Event{Type: EventToolStart, ToolCall: &call})
			result := a.runTool(ctx, call)
			emit(Event{Type: EventToolResult, Result: &result})
			a.append(Message{
				Role:       RoleTool,
				Content:    result.Content,
				ToolCallID: call.ID,
				ToolName:   call.Name,
				Timestamp:  nowMilli(),
			}, emit)
		}
		emit(Event{Type: EventTurnEnd})
	}

	err := fmt.Errorf("agent stopped after %d turns without a final answer", maxTurns)
	emit(Event{Type: EventError, Error: err.Error()})
	return err
}

// maxRetries returns the effective retry budget.
func (a *Agent) maxRetries() int {
	if a.MaxRetries < 0 {
		return 0
	}
	if a.MaxRetries == 0 {
		return DefaultMaxRetries
	}
	return a.MaxRetries
}

func (a *Agent) retryBaseDelay() time.Duration {
	if a.RetryBaseDelay <= 0 {
		return DefaultRetryBaseDelay
	}
	return a.RetryBaseDelay
}

// streamWithRetry asks the provider for the next assistant message, retrying
// transient failures (a connection reset, a 429, a 5xx) with exponential
// backoff. A permanent failure, a cancelled context, and a failure that happened
// after part of the answer had already been streamed are returned immediately:
// retrying those would duplicate the text the user has already seen.
func (a *Agent) streamWithRetry(ctx context.Context, emit func(Event), onText func(string)) (Message, error) {
	var streamed strings.Builder
	send := func() (Message, error) {
		streamed.Reset()
		return a.Provider.Stream(ctx, a.requestMessages(), a.toolSpecs(), func(delta string) {
			streamed.WriteString(delta)
			onText(delta)
		})
	}
	return a.retryTransient(ctx, send, emit, func() int { return streamed.Len() })
}

// retryTransient applies the retry policy to one provider call: a transient
// failure is waited out and retried, and each retry is reported to emit. A nil
// emit is how a request that is not a turn (a summarization) is retried without
// producing the turn's retry notices. streamed reports how much of the answer
// the failed attempt already delivered; retrying would show it twice.
func (a *Agent) retryTransient(ctx context.Context, send func() (Message, error), emit func(Event), streamed func() int) (Message, error) {
	max := a.maxRetries()
	delay := a.retryBaseDelay()
	for attempt := 0; ; attempt++ {
		msg, err := send()
		if err == nil {
			return msg, nil
		}
		if streamed != nil {
			if n := streamed(); n > 0 {
				return msg, fmt.Errorf("%w (the answer was cut off after %d characters)", err, n)
			}
		}

		var retryable *RetryableError
		if attempt >= max || ctx.Err() != nil || !errors.As(err, &retryable) {
			return msg, err
		}

		wait := delay
		if retryable.RetryAfter > 0 {
			wait = retryable.RetryAfter
		}
		if wait > maxRetryDelay {
			wait = maxRetryDelay
		}
		if emit != nil {
			emit(Event{
				Type:        EventRetry,
				Error:       err.Error(),
				Attempt:     attempt + 1,
				MaxAttempts: max,
				RetryInMS:   int(wait / time.Millisecond),
			})
		}
		sleep := a.Sleep
		if sleep == nil {
			sleep = sleepCtx
		}
		if serr := sleep(ctx, wait); serr != nil {
			return msg, serr
		}
		if delay < maxRetryDelay {
			delay *= 2
		}
	}
}

func (a *Agent) append(m Message, emit func(Event)) {
	a.History = append(a.History, m)
	if a.OnMessage != nil {
		a.OnMessage(m)
	}
	msg := m
	emit(Event{Type: "message", Message: &msg})
}

func (a *Agent) requestMessages() []Message {
	msgs := make([]Message, 0, len(a.History)+2)
	if strings.TrimSpace(a.SystemPrompt) != "" {
		msgs = append(msgs, Message{Role: RoleSystem, Content: a.SystemPrompt})
	}
	msgs = append(msgs, a.contextMessages()...)
	return msgs
}

func (a *Agent) toolSpecs() []ToolSpec {
	specs := make([]ToolSpec, 0, len(a.Tools))
	for _, t := range a.Tools {
		specs = append(specs, ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return specs
}

func (a *Agent) runTool(ctx context.Context, call ToolCall) ToolResult {
	start := time.Now()
	var tool Tool
	for _, t := range a.Tools {
		if t.Name() == call.Name {
			tool = t
			break
		}
	}
	if tool == nil {
		return ToolResult{
			Name: call.Name,
			Content: fmt.Sprintf("Error: unknown tool %q. Available tools: %s",
				call.Name, strings.Join(a.ToolNames(), ", ")),
			IsError:    true,
			DurationMS: time.Since(start).Milliseconds(),
		}
	}
	args := json.RawMessage(call.Arguments)
	if len(strings.TrimSpace(call.Arguments)) == 0 {
		args = json.RawMessage("{}")
	}
	out, err := tool.Run(ctx, args)
	res := ToolResult{Name: call.Name, DurationMS: time.Since(start).Milliseconds()}
	if err != nil {
		res.IsError = true
		if out != "" {
			res.Content = truncate(out+"\nError: "+err.Error(), MaxToolResultChars)
		} else {
			res.Content = truncate("Error: "+err.Error(), MaxToolResultChars)
		}
		return res
	}
	if strings.TrimSpace(out) == "" {
		out = "(no output)"
	}
	res.Content = truncate(out, MaxToolResultChars)
	return res
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	head := max * 3 / 4
	tail := max - head
	return s[:head] +
		fmt.Sprintf("\n\n... [%d bytes truncated] ...\n\n", len(s)-max) +
		s[len(s)-tail:]
}

// BuildSystemPrompt assembles the default system prompt plus any discovered
// context files / extra instructions.
func BuildSystemPrompt(cwd string, extra string) string {
	var b strings.Builder
	b.WriteString(`You are sal, a minimal coding agent that works in the user's terminal.

You help with programming, research, writing, and file work by inspecting the
working directory, running commands, and editing files. You work autonomously:
you do not ask for permission before each tool call, so be careful and
deliberate about what you change.

Guidelines:
- Answer the user's request directly. Prefer doing the work over describing it.
- Use the read tool before editing a file you have not seen.
- Use the edit tool for targeted changes and write for new files or full rewrites.
- Use the bash tool for builds, tests, git, and anything else the shell does.
- Use grep/find/ls to locate code instead of guessing paths.
- Keep responses concise; report what changed and what you verified.
- If a tool fails, read the error and adjust; do not repeat the same call.
- Never invent command output, file contents, or test results.`)
	b.WriteString("\n\nEnvironment:\n")
	b.WriteString("- Working directory: " + cwd + "\n")
	b.WriteString("- OS/arch: " + runtime.GOOS + "/" + runtime.GOARCH + "\n")
	b.WriteString("- Date: " + time.Now().Format("2006-01-02") + "\n")
	if strings.TrimSpace(extra) != "" {
		b.WriteString("\n" + strings.TrimSpace(extra) + "\n")
	}
	return b.String()
}
