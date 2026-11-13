// Package tui implements sal's interactive terminal interface with bubbletea:
// a header, a scrollable transcript of messages and tool calls, a multi-line
// editor, and a footer showing directory, model, session and token usage.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/bothought/sal/internal/agent"
)

// Agent is the slice of *agent.Agent the TUI drives. Keeping it an interface
// makes the model unit-testable with a scripted fake.
type Agent interface {
	RunPrompt(ctx context.Context, prompt string, emit func(agent.Event)) error
	ModelName() string
	SetModelName(string) bool
	ProviderName() string
	Messages() []agent.Message
	Reset()
	ToolNames() []string
	// ContextInfo reports the size of the next request against the window.
	ContextInfo() agent.ContextInfo
	// Compact replaces older messages with a summary.
	Compact(ctx context.Context, instructions string, automatic bool) (agent.CompactionInfo, error)
	// Summary is the active summary, or "" when there is none.
	Summary() string
	// ActiveContext is what the model is sent after a compaction: the summary,
	// the messages after it, and how many messages the summary replaces.
	ActiveContext() (summary string, messages []agent.Message, summarized int)
}

// Options configures the TUI.
type Options struct {
	Agent         Agent
	Cwd           string
	Version       string
	InitialPrompt string
	// SessionInfo describes the current session (/session command).
	SessionInfo func() string
	// SessionID reports the id of the current session, so a turn that finished
	// after the console moved on can be told apart from the current one.
	SessionID func() string
	// NewSession starts this directory over and returns a description.
	NewSession func() (string, error)
	// OpenSession switches to another directory's conversation ("" = the one
	// the console started in) and returns a description.
	OpenSession func(dir string) (string, error)
}

// Block kinds in the transcript.
const (
	kindUser      = "user"
	kindAssistant = "assistant"
	kindTool      = "tool"
	kindSystem    = "system"
	kindError     = "error"
)

const doneEventType = "__run_done"

type block struct {
	kind  string
	title string
	body  string
	done  bool
	isErr bool
}

// Model is the bubbletea model.
type Model struct {
	opts   Options
	vp     viewport.Model
	ta     textarea.Model
	sp     spinner.Model
	events chan agent.Event

	blocks  []block
	cur     int // index of the streaming assistant block, -1 if none
	curTool int

	running  bool
	aborting bool
	cancel   context.CancelFunc
	queued   []string
	width    int
	height   int
	ready    bool

	usageIn   int
	usageOut  int
	turns     int
	ctx       agent.ContextInfo
	lastErr   error
	status    string
	startedAt time.Time
}

type eventMsg agent.Event
type runDoneMsg struct{ err error }

// New builds the TUI model.
func New(opts Options) *Model {
	ta := textarea.New()
	ta.Placeholder = "Ask sal to do something…  (/help for commands, esc aborts)"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.Prompt = "› "
	// Enter submits; newline needs an explicit modifier.
	ta.KeyMap.InsertNewline.SetKeys("alt+enter", "shift+enter", "ctrl+j")
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	m := &Model{
		opts:    opts,
		ta:      ta,
		sp:      sp,
		cur:     -1,
		curTool: -1,
		events:  make(chan agent.Event, 512),
		status:  "ready",
	}
	if opts.Version == "" {
		m.opts.Version = "dev"
	}
	if opts.Agent != nil {
		m.ctx = opts.Agent.ContextInfo()
	}
	m.addBlock(kindSystem, "", startupText(opts))
	return m
}

func startupText(opts Options) string {
	var b strings.Builder
	b.WriteString(headerLine(opts))
	if opts.Agent != nil {
		tools := opts.Agent.ToolNames()
		b.WriteString("\ntools: " + strings.Join(tools, ", "))
	}
	b.WriteString("\nenter submit · alt+enter newline · esc abort · pgup/pgdn scroll · ctrl+c quit")
	b.WriteString("\n/help /model /new /session /tools /context /compact /clear /quit")
	return b.String()
}

func headerLine(opts Options) string {
	model := "no model"
	provider := ""
	if opts.Agent != nil {
		if opts.Agent.ModelName() != "" {
			model = opts.Agent.ModelName()
		}
		provider = opts.Agent.ProviderName()
	}
	if provider != "" {
		return fmt.Sprintf("sal %s — %s (%s)", opts.Version, model, provider)
	}
	return fmt.Sprintf("sal %s — %s", opts.Version, model)
}

// Init implements tea.Model. An initial prompt supplied on the command line
// starts the first run here.
func (m *Model) Init() tea.Cmd {
	if strings.TrimSpace(m.opts.InitialPrompt) != "" {
		prompt := m.opts.InitialPrompt
		m.opts.InitialPrompt = ""
		m.addBlock(kindUser, "", prompt)
		return m.startRun(prompt)
	}
	return nil
}

func waitForEvent(ch <-chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return runDoneMsg{}
		}
		return eventMsg(ev)
	}
}

func (m *Model) startRun(prompt string) tea.Cmd {
	m.running = true
	m.startedAt = time.Now()
	m.status = "working"
	m.lastErr = nil
	m.cur = -1
	m.curTool = -1
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	ch := m.events
	ag := m.opts.Agent
	p := prompt
	// The turn belongs to the current conversation: remember which one, so a
	// prompt queued while it runs resumes the right session (after a /new the
	// queue is emptied to keep the turns of one conversation together).
	sid := m.sessionID()
	go func() {
		var err error
		if ag == nil {
			err = fmt.Errorf("no agent configured")
		} else {
			err = ag.RunPrompt(ctx, p, func(ev agent.Event) {
				select {
				case ch <- ev:
				case <-ctx.Done():
				}
			})
		}
		var msg agent.Event
		if sid != m.sessionID() {
			// The session was switched while this turn ran: its result belongs
			// to a conversation the console has left, so only unblock the UI.
			err = nil
			msg = agent.Event{Type: doneEventType}
		} else {
			msg = agent.Event{Type: doneEventType}
			if err != nil {
				msg.Error = err.Error()
			}
		}
		select {
		case ch <- msg:
		case <-time.After(2 * time.Second):
		}
	}()
	return tea.Batch(waitForEvent(ch), m.sp.Tick)
}

// sessionID reports the id of the session the console is showing, or "" when
// sessions are disabled.
func (m *Model) sessionID() string {
	if m.opts.SessionID == nil {
		return ""
	}
	return m.opts.SessionID()
}

// Update implements tea.Model. All Model methods use pointer receivers so the
// value returned to bubbletea is always the same *Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.ready = true
		m.refresh()
		return m, nil

	case spinner.TickMsg:
		if !m.running {
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case eventMsg:
		ev := agent.Event(msg)
		if ev.Type == doneEventType {
			var err error
			if ev.Error != "" {
				err = errors.New(ev.Error)
			}
			return m.finishRun(err)
		}
		m.handleEvent(ev)
		return m, waitForEvent(m.events)

	case runDoneMsg:
		return m.finishRun(msg.err)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		if m.running && m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	case "esc":
		if m.running {
			if m.cancel != nil {
				m.cancel()
			}
			m.aborting = true
			m.status = "aborting"
			m.addBlock(kindSystem, "", "aborted by user")
			m.refresh()
			return m, nil
		}
		m.ta.SetValue("")
		return m, nil
	case "ctrl+l":
		m.blocks = nil
		m.refresh()
		return m, nil
	case "pgup", "pgdown", "alt+up", "alt+down":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case "enter":
		text := strings.TrimSpace(m.ta.Value())
		if text == "" {
			return m, nil
		}
		m.ta.Reset()
		if strings.HasPrefix(text, "/") {
			m.handleCommand(text)
			m.layout()
			m.refresh()
			return m, nil
		}
		if m.running {
			m.queued = append(m.queued, text)
			m.addBlock(kindSystem, "", "queued (runs after the current turn): "+firstLine(text))
			m.refresh()
			return m, nil
		}
		// Switching sessions while the agent works aborts the turn, so the run
		// finishing may still have a queue from the old conversation; this
		// prompt is dropped to keep the turns of one conversation together.
		if len(m.queued) > 0 {
			m.addBlock(kindSystem, "", "dropped (a session switch interrupted the queue): "+firstLine(text))
			m.refresh()
			return m, nil
		}
		m.addBlock(kindUser, "", text)
		m.refresh()
		return m, m.startRun(text)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// finishRun clears the working state, reports a failure, and starts the next
// queued prompt if there is one.
func (m *Model) finishRun(err error) (tea.Model, tea.Cmd) {
	// The agent reports its error as text over the event channel, so a
	// stringified "context canceled" no longer matches errors.Is. Track the
	// abort locally instead of re-deriving cancellation from the message.
	aborted := m.aborting
	m.aborting = false
	m.running = false
	m.cur = -1
	m.curTool = -1
	m.cancel = nil
	if err != nil && !aborted && !errors.Is(err, context.Canceled) && m.lastErr == nil {
		m.lastErr = err
		m.addBlock(kindError, "run failed", err.Error())
	}
	if len(m.queued) > 0 {
		next := m.queued[0]
		m.queued = m.queued[1:]
		m.refresh()
		if strings.HasPrefix(next, "/") {
			// A queued command (only /compact can be one) runs now that the
			// turn it waited for has finished.
			m.handleCommand(next)
			m.layout()
			m.refresh()
			return m, nil
		}
		return m, m.startRun(next)
	}
	m.status = "ready"
	m.refresh()
	return m, nil
}

// replayHistory rebuilds the transcript from the conversation the agent holds,
// which is what makes a session switch show the conversation it switched to.
// Live tool results are unavailable from the history alone, so each tool call
// is one block; the next turn streams on top of them.
func (m *Model) replayHistory() {
	if m.opts.Agent == nil {
		return
	}
	summary, msgs, summarized := m.opts.Agent.ActiveContext()
	if len(msgs) == 0 && summary == "" {
		m.addBlock(kindSystem, "", "(this session has no messages yet)")
		return
	}
	if summary != "" {
		// The summarized messages are in the file but not in the model's
		// context, so the transcript shows the summary instead of replaying
		// them.
		m.addBlock(kindSystem, "summary of earlier conversation",
			fmt.Sprintf("%d earlier messages are summarized here and not replayed:\n\n%s", summarized, summary))
	}
	restored := 0
	for _, msg := range msgs {
		switch msg.Role {
		case agent.RoleUser:
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			m.addBlock(kindUser, "", msg.Content)
		case agent.RoleAssistant:
			if strings.TrimSpace(msg.Content) != "" {
				m.addBlock(kindAssistant, "", msg.Content)
			}
			for _, call := range msg.ToolCalls {
				m.addBlock(kindTool, call.Name+"("+summarizeArgs(call.Arguments)+")", "")
				restored++
			}
		}
	}
	if restored > 0 {
		m.addBlock(kindSystem, "", fmt.Sprintf("restored %d messages (tool output is not shown)", len(msgs)))
	}
}

// handleEvent folds one agent event into the transcript.
func (m *Model) handleEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventTextDelta:
		if m.cur < 0 {
			m.addBlock(kindAssistant, "", "")
			m.cur = len(m.blocks) - 1
		}
		m.blocks[m.cur].body += ev.Text
	case agent.EventToolStart:
		if ev.ToolCall != nil {
			title := ev.ToolCall.Name + "(" + summarizeArgs(ev.ToolCall.Arguments) + ")"
			m.addBlock(kindTool, title, "")
			m.curTool = len(m.blocks) - 1
		}
		m.cur = -1
	case agent.EventToolResult:
		if ev.Result != nil && m.curTool >= 0 && m.curTool < len(m.blocks) {
			b := &m.blocks[m.curTool]
			b.body = ev.Result.Content
			b.done = true
			b.isErr = ev.Result.IsError
			if ev.Result.DurationMS > 0 {
				b.title = fmt.Sprintf("%s  [%dms]", b.title, ev.Result.DurationMS)
			}
		}
		m.curTool = -1
		m.cur = -1
	case agent.EventTurnEnd:
		m.cur = -1
		m.turns++
	case agent.EventUsage:
		if ev.Usage != nil {
			m.usageIn += ev.Usage.InputTokens
			m.usageOut += ev.Usage.OutputTokens
		}
	case agent.EventError:
		m.lastErr = fmt.Errorf("%s", ev.Error)
		m.addBlock(kindError, "error", ev.Error)
	case agent.EventRetry:
		m.addBlock(kindSystem, "retrying",
			fmt.Sprintf("%s — attempt %d of %d in %.1fs", ev.Error, ev.Attempt, ev.MaxAttempts, float64(ev.RetryInMS)/1000))
	case agent.EventContext:
		if ev.Context != nil {
			m.ctx = *ev.Context
		}
	case agent.EventCompaction:
		if ev.Compaction != nil {
			m.addBlock(kindSystem, "context compacted", compactionSummary(*ev.Compaction))
		}
	}
	m.refresh()
}

func (m *Model) handleCommand(text string) {
	fields := strings.Fields(text)
	cmd := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	args := fields[1:]
	switch cmd {
	case "help", "?":
		m.addBlock(kindSystem, "help", helpText())
	case "model":
		if len(args) == 0 {
			m.addBlock(kindSystem, "model", "current model: "+m.opts.Agent.ModelName())
			return
		}
		name := strings.Join(args, " ")
		if m.opts.Agent.SetModelName(name) {
			m.addBlock(kindSystem, "model", "switched to "+name)
			// The window belongs to the model, so the figure must follow it.
			if !m.running {
				m.ctx = m.opts.Agent.ContextInfo()
			}
			if m.opts.SessionInfo != nil {
				// session model bookkeeping is the caller's business
				_ = m.opts.SessionInfo
			}
		} else {
			m.addBlock(kindError, "model", "this provider cannot switch models at runtime")
		}
	case "new":
		// With a directory, /new continues that directory's conversation; with
		// none, it starts this one over.
		if len(args) > 0 {
			m.openDirectory(strings.Join(args, " "))
			return
		}
		if m.running {
			// The web console refuses this too. The turn's messages belong to
			// the conversation being archived, and its result would be written
			// into the one replacing it.
			m.addBlock(kindError, "new session", "cannot start over while a turn is running (esc aborts it)")
			return
		}
		m.resetConversation()
		desc := ""
		if m.opts.NewSession != nil {
			if d, err := m.opts.NewSession(); err != nil {
				m.addBlock(kindError, "new session", err.Error())
				return
			} else {
				desc = d
			}
		}
		m.addBlock(kindSystem, "new session", "started over "+desc)
	case "context":
		// The figures the console holds, not a fresh read of the agent: a run
		// in flight is mutating it, and the event stream has kept m.ctx current.
		if len(args) > 0 && strings.EqualFold(args[0], "summary") {
			m.addBlock(kindSystem, "context summary", contextSummaryText(m.ctx))
			return
		}
		if m.running {
			m.addBlock(kindSystem, "context", "the run is in flight; the footer has the current figure")
			return
		}
		m.ctx = m.opts.Agent.ContextInfo()
		m.addBlock(kindSystem, "context", contextReport(m.opts.Agent.ModelName(), m.ctx))
	case "compact":
		if m.running {
			// Compaction needs a stable history, so it runs after the turn
			// rather than aborting the user's work.
			m.queued = append(m.queued, strings.TrimSpace(text))
			m.addBlock(kindSystem, "", "queued (compaction runs after the current turn)")
			return
		}
		info, err := m.opts.Agent.Compact(context.Background(), strings.Join(args, " "), false)
		if err != nil && info.Error == "" && info.Note == "" {
			info.Error = err.Error()
		}
		m.addBlock(kindSystem, "context compacted", compactionSummary(info))
		m.ctx = m.opts.Agent.ContextInfo()
	case "session":
		info := "no session (ephemeral mode)"
		if m.opts.SessionInfo != nil {
			info = m.opts.SessionInfo()
		}
		m.addBlock(kindSystem, "session", info)

	case "tools":
		m.addBlock(kindSystem, "tools", strings.Join(m.opts.Agent.ToolNames(), ", "))
	case "clear":
		m.blocks = nil
	case "quit", "exit":
		m.addBlock(kindSystem, "", "use ctrl+c to quit")
	default:
		m.addBlock(kindError, "", "unknown command: /"+cmd+" (try /help)")
	}
}

func helpText() string {
	return `commands:
  /help              show this help
  /model [name]      show or switch the model
  /new [dir]         start this directory over, or continue another's
  /session           show the current session
  /tools             list enabled tools
  /context [summary] show the context window, or the active summary
  /compact [notes]   summarize older messages to free context
  /clear             clear the transcript (ctrl+l)
  /quit              quit (ctrl+c)
keys:
  enter              submit (queues while the agent is working)
  alt+enter          newline
  esc                abort the current run
  pgup/pgdn          scroll the transcript
  ctrl+c             quit`
}

// resetConversation clears the agent's conversation and the console. It is what
// starting over needs; a console that is instead switching to an existing
// conversation clears only its own state (see clearTranscript).
func (m *Model) resetConversation() {
	m.opts.Agent.Reset()
	// The indicator resets with the conversation, and no summary carries over.
	m.ctx = m.opts.Agent.ContextInfo()
	m.clearTranscript()
}

// clearTranscript drops the visible transcript and the per-conversation
// counters, leaving the agent's conversation alone.
func (m *Model) clearTranscript() {
	m.blocks = nil
	m.turns = 0
	m.usageIn, m.usageOut = 0, 0
	m.queued = nil
	m.cur, m.curTool = -1, -1
	m.lastErr = nil
}

// openDirectory switches the console to another directory's conversation and
// replays it into the transcript.
func (m *Model) openDirectory(dir string) {
	if m.opts.OpenSession == nil {
		m.addBlock(kindSystem, "session", "sessions are disabled (--no-session)")
		return
	}
	// Opening another conversation aborts the turn in flight; its queue belonged
	// to the conversation being left. Marking it as an abort keeps the
	// cancellation from being reported as a failure of the one being opened.
	if m.running && m.cancel != nil {
		m.aborting = true
		m.cancel()
		m.queued = nil
	}
	desc, err := m.opts.OpenSession(dir)
	if err != nil {
		m.addBlock(kindError, "session", err.Error())
		return
	}
	// Clear the console without touching the agent: OpenSession has already
	// installed the new conversation in it, and resetting would discard it.
	m.clearTranscript()
	if m.opts.Agent != nil {
		m.ctx = m.opts.Agent.ContextInfo()
	}
	m.replayHistory()
	msg := "showing " + shortenHome(dir)
	if desc != "" {
		msg += " " + desc
	}
	m.addBlock(kindSystem, "session", msg)
	m.refresh()
}

func (m *Model) addBlock(kind, title, body string) {
	m.blocks = append(m.blocks, block{kind: kind, title: title, body: body, done: true})
	m.cur = -1
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 60)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// summarizeArgs renders tool arguments as a short one-line summary.
func summarizeArgs(raw string) string {
	var generic map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		return truncate(strings.ReplaceAll(raw, "\n", " "), 60)
	}
	for _, key := range []string{"path", "command", "pattern", "query"} {
		if v, ok := generic[key].(string); ok && v != "" {
			return truncate(strings.ReplaceAll(v, "\n", " "), 60)
		}
	}
	return truncate(strings.ReplaceAll(raw, "\n", " "), 60)
}

func (m *Model) layout() {
	editorHeight := m.ta.Height() + 2 // textarea + border
	avail := m.height - editorHeight - 3
	if avail < 1 {
		avail = 1
	}
	if !m.ready || m.vp.Width == 0 {
		m.vp = viewport.New(m.width, avail)
	} else {
		m.vp.Width = m.width
		m.vp.Height = avail
	}
	m.ta.SetWidth(m.width - 4)
}

// refresh re-renders the transcript into the viewport.
func (m *Model) refresh() {
	if !m.ready {
		return
	}
	content := m.renderBlocks()
	m.vp.SetContent(content)
	m.vp.GotoBottom()
}

// View implements tea.Model.
func (m *Model) View() string {
	if !m.ready {
		return "starting sal…"
	}
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render(
		fmt.Sprintf("sal %s", m.opts.Version)))
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(
		"  " + shortenHome(m.opts.Cwd)))
	b.WriteString("\n")
	b.WriteString(m.vp.View())
	b.WriteString("\n")
	b.WriteString(m.viewEditor())
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

// viewEditor renders the input box, coloured while a turn is running.
func (m Model) viewEditor() string {
	borderColor := "240"
	if m.running {
		borderColor = "205"
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Width(m.width - 2).
		Render(m.ta.View())
}

func (m Model) footer() string {
	left := shortenHome(m.opts.Cwd)
	model := "no model"
	if m.opts.Agent != nil && m.opts.Agent.ModelName() != "" {
		model = m.opts.Agent.ModelName()
	}
	status := m.status
	if m.running {
		status = m.sp.View() + " working"
	}
	if m.running && m.ctx.Compacting {
		status = m.sp.View() + " compacting…"
	}
	// The context segment sits between the status and the counters, and is
	// coloured by how full the window is.
	ctxText := m.contextSegment()
	right := fmt.Sprintf("%s · %s · %s · ↑%s ↓%s · %d turns",
		model, status, ctxText, humanCount(m.usageIn), humanCount(m.usageOut), m.turns)
	width := m.width
	if width <= 0 {
		width = 80
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
		Render(left + strings.Repeat(" ", gap) + right)
}

// contextSegment renders "ctx used/limit percent", marked "~" while the figure
// is estimated. Above the window it says how much it is over by, since a
// percentage past 100 is less useful than the gap.
func (m Model) contextSegment() string {
	c := m.ctx
	if c.Limit <= 0 && c.Used <= 0 {
		return "ctx —"
	}
	tick := ""
	if !c.Exact {
		tick = "~"
	}
	tail := ""
	if c.Limit > 0 {
		if c.Used > c.Limit {
			tail = fmt.Sprintf(" over by %s", humanCount(c.Used-c.Limit))
		} else {
			tail = fmt.Sprintf(" %d%%", c.Used*100/c.Limit)
		}
	}
	text := fmt.Sprintf("ctx %s%s/%s%s", tick, humanCount(c.Used), humanCount(c.Limit), tail)
	return lipgloss.NewStyle().Foreground(lipgloss.Color(contextColor(c))).Render(text)
}

// contextColor maps the occupancy of the window onto the footer palette: the
// same warning and error colours the transcript already uses.
func contextColor(c agent.ContextInfo) string {
	if c.Limit <= 0 {
		return "240"
	}
	pct := c.Used * 100 / c.Limit
	switch {
	case pct >= 100:
		return "196"
	case c.CompactAt > 0 && pct >= c.CompactAt:
		return "203"
	case pct >= 70:
		return "214"
	default:
		return "240"
	}
}

// compactionSummary describes a compaction in one block of text.
func compactionSummary(info agent.CompactionInfo) string {
	switch {
	case info.Error != "":
		return "compaction failed, the context is unchanged: " + info.Error
	case info.Messages == 0:
		return "nothing to compact yet: the conversation is still short"
	}
	what := "requested"
	if info.Automatic {
		what = "automatic"
	}
	kept := info.KeptTurns
	if kept == 0 {
		kept = agent.DefaultKeepTurns
	}
	line := fmt.Sprintf("%d messages summarized · %s → %s tokens · the last %d turns kept (%s)",
		info.Messages, humanCount(info.Before), humanCount(info.After), kept, what)
	if info.Shortened > 0 {
		line += fmt.Sprintf("\n%d tool results shortened to fit the window", info.Shortened)
	}
	if info.Note != "" {
		line += "\n" + info.Note
	}
	return line
}

// contextReport renders the /context breakdown, so the footer figure is
// explainable: which part of the context is large, and whether it is measured.
func contextReport(model string, info agent.ContextInfo) string {
	if model == "" {
		model = "no model"
	}
	how := "estimated"
	if info.Exact {
		how = "measured"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  model         %s\n", model)
	fmt.Fprintf(&b, "  window        %s (%s)\n", humanCount(info.Limit), limitSourceText(info.Source))
	fmt.Fprintf(&b, "  used          %s tokens (%d%%, %s)\n", humanCount(info.Used), percent(info.Used, info.Limit), how)
	fmt.Fprintf(&b, "    system        %s\n", humanCount(info.System))
	fmt.Fprintf(&b, "    tools         %s\n", humanCount(info.Tools))
	fmt.Fprintf(&b, "    messages      %s\n", humanCount(info.Recent+info.Summary))
	if info.Summary > 0 {
		fmt.Fprintf(&b, "      summary     %s (%d earlier messages)\n", humanCount(info.Summary), info.SummaryMessages)
	}
	fmt.Fprintf(&b, "      recent      %s (%d messages, %d turns kept)\n", humanCount(info.Recent), info.RecentMessages, info.KeepTurns)
	headroom := info.Limit - info.Used
	if headroom < 0 {
		headroom = 0
	}
	fmt.Fprintf(&b, "  headroom      %s\n", humanCount(headroom))
	if info.AutoCompact {
		fmt.Fprintf(&b, "  auto-compact  on, at %d%% (%s)\n", info.CompactAt, humanCount(info.Limit*info.CompactAt/100))
	} else {
		fmt.Fprintf(&b, "  auto-compact  off (/compact to compact now)\n")
	}
	if info.LastInput > 0 || info.LastOutput > 0 {
		fmt.Fprintf(&b, "  last request  %s input + %s output (measured)\n",
			humanCount(info.LastInput), humanCount(info.LastOutput))
	}
	return b.String()
}

func contextSummaryText(info agent.ContextInfo) string {
	if s := strings.TrimSpace(info.SummaryText); s != "" {
		return s
	}
	return "(no active summary: the whole conversation is in context)"
}

func limitSourceText(source string) string {
	switch source {
	case agent.LimitFromConfig:
		return "configured"
	case agent.LimitFromProvider:
		return "from the endpoint"
	case agent.LimitFromTable:
		return "from the built-in table"
	case agent.LimitFromDefault:
		return "assumed"
	}
	return source
}

func percent(used, limit int) int {
	if limit <= 0 {
		return 0
	}
	return used * 100 / limit
}

func (m Model) renderBlocks() string {
	if len(m.blocks) == 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("(empty conversation — type a task)")
	}
	var parts []string
	for _, b := range m.blocks {
		parts = append(parts, m.renderBlock(b))
	}
	return strings.Join(parts, "\n")
}

func (m Model) renderBlock(b block) string {
	width := m.width - 4
	if width < 20 {
		width = 20
	}
	switch b.kind {
	case kindUser:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")).
			Width(width).Render("› " + b.body)
	case kindAssistant:
		out := lipgloss.NewStyle().Width(width).Render(b.body)
		if !b.done && b.body == "" {
			out = m.sp.View() + " thinking…"
		}
		return out
	case kindTool:
		name := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("⚙ " + b.title)
		body := preview(b.body)
		if body == "" {
			body = "(running…)"
		}
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Width(width)
		if b.isErr {
			style = style.Foreground(lipgloss.Color("203"))
		}
		return name + "\n" + style.Render(body)
	case kindError:
		head := "✗ " + b.title
		if b.title == "" {
			head = "✗ error"
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Width(width).
			Render(head + "\n" + b.body)
	default:
		head := b.title
		if head != "" {
			head += "\n"
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Width(width).
			Render(head + b.body)
	}
}

// preview limits a tool result to its first lines plus a summary of the rest.
func preview(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	const maxLines = 12
	if len(lines) <= maxLines {
		return s
	}
	return strings.Join(lines[:maxLines], "\n") +
		fmt.Sprintf("\n… (%d more lines)", len(lines)-maxLines)
}

func shortenHome(p string) string {
	if home, err := homeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// humanCount abbreviates a token count for the footer: whole thousands and
// millions lose their decimal ("128k", not "128.0k"), and a fractional value
// keeps one digit.
func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		if n%1_000_000 == 0 {
			return fmt.Sprintf("%dM", n/1_000_000)
		}
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		if n%1_000 == 0 {
			return fmt.Sprintf("%dk", n/1_000)
		}
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
