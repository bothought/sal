package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bothought/sal/internal/agent"
)

// fakeAgent is a scripted stand-in for *agent.Agent.
type fakeAgent struct {
	mu         sync.Mutex
	script     []agent.Event
	blocking   bool
	prompts    []string
	model      string
	aborted    bool
	failWith   error
	history    []agent.Message
	ctxInfo    agent.ContextInfo
	summary    string
	summarized int
	compacts   []string
}

func (f *fakeAgent) RunPrompt(ctx context.Context, prompt string, emit func(agent.Event)) error {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	script := append([]agent.Event(nil), f.script...)
	failWith := f.failWith
	blocking := f.blocking
	f.mu.Unlock()

	for _, ev := range script {
		emit(ev)
	}
	if blocking {
		<-ctx.Done()
		f.mu.Lock()
		f.aborted = true
		f.mu.Unlock()
		return ctx.Err()
	}
	if failWith != nil {
		emit(agent.Event{Type: agent.EventError, Error: failWith.Error()})
		return failWith
	}
	return nil
}

func (f *fakeAgent) ModelName() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.model == "" {
		return "fake-model"
	}
	return f.model
}
func (f *fakeAgent) SetModelName(m string) bool { f.mu.Lock(); f.model = m; f.mu.Unlock(); return true }
func (f *fakeAgent) ProviderName() string       { return "fake" }
func (f *fakeAgent) ToolNames() []string        { return []string{"read", "bash", "edit", "write"} }

func (f *fakeAgent) Messages() []agent.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Message(nil), f.history...)
}

func (f *fakeAgent) Reset() { f.mu.Lock(); f.history = nil; f.mu.Unlock() }

func (f *fakeAgent) ContextInfo() agent.ContextInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctxInfo
}

func (f *fakeAgent) Summary() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary
}

func (f *fakeAgent) ActiveContext() (string, []agent.Message, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary, append([]agent.Message(nil), f.history...), f.summarized
}

func (f *fakeAgent) Compact(_ context.Context, instructions string, automatic bool) (agent.CompactionInfo, error) {
	f.mu.Lock()
	f.compacts = append(f.compacts, instructions)
	before := f.ctxInfo
	f.mu.Unlock()
	return agent.CompactionInfo{Messages: 12, Before: 105000, After: 24000, Automatic: automatic,
		Shortened: before.SummaryMessages}, nil
}

func (f *fakeAgent) setHistory(msgs ...agent.Message) {
	f.mu.Lock()
	f.history = msgs
	f.mu.Unlock()
}

func (f *fakeAgent) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func (f *fakeAgent) wasAborted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aborted
}

// newTestModel builds a sized model plus its event channel.
func newTestModel(t *testing.T, a Agent) (*Model, chan agent.Event) {
	t.Helper()
	m := New(Options{Agent: a, Cwd: "/work/project", Version: "test"})
	ch := m.events
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return tm.(*Model), ch
}

// pump drains the agent's events into the model until the run finishes.
func pump(t *testing.T, m *Model, ch chan agent.Event) *Model {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			var tm tea.Model = m
			tm, _ = tm.Update(eventMsg(ev))
			m = tm.(*Model)
			if ev.Type == doneEventType {
				return m
			}
		case <-deadline:
			t.Fatal("timed out waiting for the run to finish")
		}
	}
}

func typeAndEnter(t *testing.T, m *Model, text string) (*Model, tea.Cmd) {
	t.Helper()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
	tm, cmd := tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return tm.(*Model), cmd
}

func TestSubmitRunsAgentAndRendersTranscript(t *testing.T) {
	fa := &fakeAgent{script: []agent.Event{
		{Type: agent.EventTextDelta, Text: "Sure, "},
		{Type: agent.EventTextDelta, Text: "creating the file."},
		{Type: agent.EventToolStart, ToolCall: &agent.ToolCall{ID: "1", Name: "write", Arguments: `{"path":"hello.txt","content":"hi"}`}},
		{Type: agent.EventToolResult, Result: &agent.ToolResult{Name: "write", Content: "wrote 2 bytes to hello.txt", DurationMS: 4}},
		{Type: agent.EventTurnEnd},
		{Type: agent.EventUsage, Usage: &agent.Usage{InputTokens: 1200, OutputTokens: 34}},
	}}
	m, ch := newTestModel(t, fa)

	if !strings.HasPrefix(m.View(), "sal test") {
		t.Fatalf("header missing from the view:\n%s", m.View())
	}

	m, cmd := typeAndEnter(t, m, "create hello.txt")
	if cmd == nil {
		t.Fatal("submitting should start the run and return a command")
	}
	if !m.running {
		t.Fatal("model should be marked as running")
	}

	m = pump(t, m, ch)

	if m.running {
		t.Fatal("running should be cleared once the run finishes")
	}
	if m.status != "ready" {
		t.Fatalf("status should return to ready, got %q", m.status)
	}
	if fa.promptCount() != 1 {
		t.Fatalf("expected exactly one prompt, got %d", fa.promptCount())
	}

	view := m.View()
	for _, want := range []string{"create hello.txt", "creating the file.", "write(hello.txt)", "wrote 2 bytes to hello.txt"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "↑1.2k ↓34") {
		t.Fatalf("footer should show token usage:\n%s", view)
	}
	if !strings.Contains(view, "fake-model") {
		t.Fatalf("footer should show the model:\n%s", view)
	}
}

func TestToolResultErrorsAreMarked(t *testing.T) {
	fa := &fakeAgent{script: []agent.Event{
		{Type: agent.EventToolStart, ToolCall: &agent.ToolCall{ID: "1", Name: "bash", Arguments: `{"command":"exit 3"}`}},
		{Type: agent.EventToolResult, Result: &agent.ToolResult{Name: "bash", Content: "exit code 3", IsError: true}},
	}}
	m, _ := newTestModel(t, fa)
	m, _ = typeAndEnter(t, m, "run it")
	// fold the scripted events directly: deterministic, no goroutine race
	for _, ev := range fa.script {
		var tm tea.Model = m
		tm, _ = tm.Update(eventMsg(ev))
		m = tm.(*Model)
	}
	if len(m.blocks) < 2 {
		t.Fatalf("expected a tool block, got %d blocks", len(m.blocks))
	}
	tb := m.blocks[len(m.blocks)-1]
	if !tb.isErr || tb.kind != kindTool {
		t.Fatalf("tool failure not marked: %+v", tb)
	}
}

func TestEscAbortsRunningAgent(t *testing.T) {
	fa := &fakeAgent{blocking: true}
	m, ch := newTestModel(t, fa)

	m, _ = typeAndEnter(t, m, "do something slow")
	if !m.running {
		t.Fatal("expected the run to be active")
	}
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(*Model)
	if !strings.Contains(m.View(), "aborted by user") {
		t.Fatalf("abort notice missing:\n%s", m.View())
	}

	// the agent observes cancellation and the run completes
	m = pump(t, m, ch)
	if !fa.wasAborted() {
		t.Fatal("agent should have seen its context cancelled")
	}
	if m.running {
		t.Fatal("abort should clear the running state")
	}
	if !strings.Contains(m.View(), "ready") {
		t.Fatalf("status should return to ready:\n%s", m.View())
	}
}

func TestQueuedPromptRunsAfterCurrentTurn(t *testing.T) {
	fa := &fakeAgent{blocking: true}
	m, ch := newTestModel(t, fa)

	m, _ = typeAndEnter(t, m, "first task")
	m, _ = typeAndEnter(t, m, "second task")
	if len(m.queued) != 1 {
		t.Fatalf("expected one queued prompt, got %d", len(m.queued))
	}
	if !strings.Contains(m.View(), "queued") {
		t.Fatalf("queue notice missing:\n%s", m.View())
	}

	// abort the first run; the queued prompt should start automatically
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = tm.(*Model)
	m = pump(t, m, ch)

	// the queued run starts in its own goroutine
	deadline := time.Now().Add(2 * time.Second)
	for fa.promptCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fa.promptCount() != 2 {
		t.Fatalf("expected the queued prompt to run as well, got %d prompts", fa.promptCount())
	}
	if fa.prompts[1] != "second task" {
		t.Fatalf("wrong queued prompt ran: %q", fa.prompts[1])
	}
	if len(m.queued) != 0 {
		t.Fatalf("queue should be drained, got %+v", m.queued)
	}
	if strings.Contains(m.View(), "run failed") {
		t.Fatalf("a user abort must not be reported as a failure:\n%s", m.View())
	}
}

func TestSlashCommands(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)

	run := func(cmd string) *Model {
		var tm tea.Model = m
		tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(cmd)})
		tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
		return tm.(*Model)
	}

	m = run("/help")
	if !strings.Contains(m.View(), "/model") || !strings.Contains(m.View(), "commands:") {
		t.Fatalf("/help output missing:\n%s", m.View())
	}
	if fa.promptCount() != 0 {
		t.Fatal("slash commands must not reach the agent")
	}

	m = run("/model sonnet")
	if !strings.Contains(m.View(), "switched to sonnet") {
		t.Fatalf("/model did not switch:\n%s", m.View())
	}
	if fa.ModelName() != "sonnet" {
		t.Fatalf("agent model not updated: %q", fa.ModelName())
	}

	m = run("/tools")
	if !strings.Contains(m.View(), "read, bash, edit, write") {
		t.Fatalf("/tools listing missing:\n%s", m.View())
	}

	sessionCalled := false
	m.opts.SessionInfo = func() string { sessionCalled = true; return "id deadbeef" }
	m = run("/session")
	if !sessionCalled || !strings.Contains(m.View(), "id deadbeef") {
		t.Fatalf("/session output missing:\n%s", m.View())
	}

	m = run("/nope")
	if !strings.Contains(m.View(), "unknown command") {
		t.Fatalf("unknown command should be reported:\n%s", m.View())
	}

	m = run("/clear")
	if len(m.blocks) != 0 {
		t.Fatalf("/clear should empty the transcript, got %d blocks", len(m.blocks))
	}
}

func TestNewCommandResetsConversation(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	newCalled := false
	m.opts.NewSession = func() (string, error) { newCalled = true; return "(session abc12345)", nil }

	m, _ = typeAndEnter(t, m, "hello")
	m, _ = typeAndEnter(t, m, "/new")

	if !newCalled {
		t.Fatal("NewSession callback should be invoked")
	}
	if len(m.blocks) != 1 || m.blocks[0].kind != kindSystem {
		t.Fatalf("expected a single system notice after /new, got %+v", m.blocks)
	}
	if !strings.Contains(m.blocks[0].body, "abc12345") {
		t.Fatalf("new session id not reported: %q", m.blocks[0].body)
	}
}

func TestRunFailureIsReported(t *testing.T) {
	fa := &fakeAgent{failWith: errors.New("provider exploded")}
	m, ch := newTestModel(t, fa)
	m, _ = typeAndEnter(t, m, "do it")
	m = pump(t, m, ch)

	view := m.View()
	if !strings.Contains(view, "provider exploded") {
		t.Fatalf("failure not shown:\n%s", view)
	}
	if m.running {
		t.Fatal("a failed run should clear the running state")
	}
}

func TestSummarizeArgs(t *testing.T) {
	cases := map[string]string{
		`{"path":"a/b.go"}`:                           "a/b.go",
		`{"command":"go test ./..."}`:                 "go test ./...",
		`{"pattern":"func main","path":"src"}`:        "src",
		`{"oldText":"x","newText":"y"}`:               `{"oldText":"x","newText":"y"}`,
		`not json`:                                    "not json",
		`{"path":"` + strings.Repeat("z", 200) + `"}`: "",
	}
	for raw, want := range cases {
		got := summarizeArgs(raw)
		if want == "" {
			if len(got) > 62 {
				t.Fatalf("long arg not truncated: %d chars", len(got))
			}
			continue
		}
		if got != want {
			t.Fatalf("summarizeArgs(%s) = %q, want %q", raw, got, want)
		}
	}
}

func press(t *testing.T, m *Model, key string) *Model {
	t.Helper()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return tm.(*Model)
}

func pressKey(t *testing.T, m *Model, k tea.KeyType) *Model {
	t.Helper()
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: k})
	return tm.(*Model)
}

func TestNewCommandDropsQueuedAndRepaintsTheEditor(t *testing.T) {
	fa := &fakeAgent{blocking: true}
	m, ch := newTestModel(t, fa)

	m, _ = typeAndEnter(t, m, "first")
	m, _ = typeAndEnter(t, m, "second")
	if len(m.queued) != 1 {
		t.Fatalf("queued = %v, want the second prompt", m.queued)
	}

	m, _ = typeAndEnter(t, m, "/new")
	if len(m.queued) != 0 {
		t.Fatalf("/new should drop the queue, got %v", m.queued)
	}
	if !strings.Contains(m.View(), "› ") {
		t.Fatalf("the editor should come back after /new:\n%s", m.View())
	}
	if fa.wasAborted() {
		t.Fatal("starting a session must not abort the running turn")
	}
	// drain the run so the test does not leave a goroutine blocked
	if m.cancel != nil {
		m.cancel()
	}
	pump(t, m, ch)
}

func TestRetryIsShownInTheTranscript(t *testing.T) {
	fa := &fakeAgent{script: []agent.Event{
		{Type: agent.EventRetry, Error: "read: connection reset by peer",
			Attempt: 2, MaxAttempts: 4, RetryInMS: 1500},
		{Type: agent.EventTextDelta, Text: "recovered"},
		{Type: agent.EventDone},
	}}
	m, ch := newTestModel(t, fa)
	m, _ = typeAndEnter(t, m, "do it")
	m = pump(t, m, ch)

	view := m.View()
	if !strings.Contains(view, "retrying") || !strings.Contains(view, "connection reset") {
		t.Fatalf("the retry should be shown:\n%s", view)
	}
	if !strings.Contains(view, "attempt 2 of 4") {
		t.Fatalf("the attempt count should be shown:\n%s", view)
	}
	if strings.Contains(view, "run failed") {
		t.Fatalf("a retry is not a failure:\n%s", view)
	}
	if m.lastErr != nil {
		t.Fatalf("a retry must not be recorded as the last error: %v", m.lastErr)
	}
}

func TestFooterShowsTheContextWindow(t *testing.T) {
	fa := &fakeAgent{ctxInfo: agent.ContextInfo{
		Used: 12400, Limit: 128000, Exact: true, AutoCompact: true, CompactAt: 80,
	}}
	m, _ := newTestModel(t, fa)
	view := m.View()
	if !strings.Contains(view, "ctx 12.4k/128k 9%") {
		t.Fatalf("the footer should show used/limit:\n%s", view)
	}

	fa.ctxInfo = agent.ContextInfo{Used: 12400, Limit: 128000, Exact: false}
	m, _ = newTestModel(t, fa)
	if view := m.View(); !strings.Contains(view, "ctx ~12.4k/128k") {
		t.Fatalf("an estimated figure should be marked:\n%s", view)
	}
}

func TestContextSegmentSeverity(t *testing.T) {
	cases := []struct {
		used, limit int
		at          int
		color       string
	}{
		{1000, 128000, 80, "240"},
		{90000, 128000, 80, "214"},
		{110000, 128000, 80, "203"},
		{130000, 128000, 80, "196"},
	}
	for _, c := range cases {
		got := contextColor(agent.ContextInfo{Used: c.used, Limit: c.limit, AutoCompact: true, CompactAt: c.at})
		if got != c.color {
			t.Fatalf("%d/%d should be colour %s, got %s", c.used, c.limit, c.color, got)
		}
	}
}

func TestContextCommandShowsTheBreakdown(t *testing.T) {
	fa := &fakeAgent{ctxInfo: agent.ContextInfo{
		Used: 12000, Limit: 128000, Exact: true, Source: agent.LimitFromTable,
		System: 1200, Tools: 800, Recent: 10000, RecentMessages: 6,
		AutoCompact: true, CompactAt: 80, KeepTurns: 3,
	}}
	m, _ := newTestModel(t, fa)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/context")})
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*Model)

	view := m.View()
	for _, want := range []string{"window", "128k", "built-in table", "used", "auto-compact", "headroom"} {
		if !strings.Contains(view, want) {
			t.Fatalf("/context output missing %q:\n%s", want, view)
		}
	}
}

func TestContextSummaryCommand(t *testing.T) {
	fa := &fakeAgent{summary: "we changed the parser",
		ctxInfo: agent.ContextInfo{SummaryText: "we changed the parser"}}
	m, _ := newTestModel(t, fa)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/context summary")})
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*Model)
	if !strings.Contains(m.View(), "we changed the parser") {
		t.Fatalf("/context summary should print the summary:\n%s", m.View())
	}
}

func TestCompactCommandRunsAndIsReported(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/compact keep the parser work")})
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*Model)

	fa.mu.Lock()
	got := append([]string(nil), fa.compacts...)
	fa.mu.Unlock()
	if len(got) != 1 || got[0] != "keep the parser work" {
		t.Fatalf("instructions were not passed: %v", got)
	}
	view := m.View()
	if !strings.Contains(view, "context compacted") || !strings.Contains(view, "12 messages summarized") {
		t.Fatalf("the compaction should be reported:\n%s", view)
	}
}

func TestCompactWhileRunningIsQueued(t *testing.T) {
	fa := &fakeAgent{blocking: true}
	m, _ := newTestModel(t, fa)
	var tm tea.Model = m
	tm, cmd := tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("work")})
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*Model)
	_ = cmd
	m.running = true // the run is in flight

	tm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/compact")})
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*Model)
	if len(m.queued) != 1 || m.queued[0] != "/compact" {
		t.Fatalf("compaction should be queued while a run is in flight: %v", m.queued)
	}
	fa.mu.Lock()
	n := len(fa.compacts)
	fa.mu.Unlock()
	if n != 0 {
		t.Fatalf("it must not compact mid-turn: %d", n)
	}
}

func TestCompactionEventAddsABlock(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	info := agent.CompactionInfo{Messages: 18, Before: 105000, After: 24000, Automatic: true}
	var tm tea.Model = m
	tm, _ = tm.Update(eventMsg(agent.Event{Type: agent.EventCompaction, Compaction: &info}))
	m = tm.(*Model)
	view := m.View()
	if !strings.Contains(view, "context compacted") || !strings.Contains(view, "105k → 24k") {
		t.Fatalf("the block should describe the compaction:\n%s", view)
	}
	if !strings.Contains(view, "automatic") {
		t.Fatalf("an automatic compaction should say so:\n%s", view)
	}
}

func TestContextSegmentSaysHowFarOverItIs(t *testing.T) {
	fa := &fakeAgent{ctxInfo: agent.ContextInfo{Used: 130000, Limit: 128000, Exact: true}}
	m, _ := newTestModel(t, fa)
	view := m.View()
	if !strings.Contains(view, "ctx 130k/128k over by 2k") {
		t.Fatalf("an over-full window should say by how much:\n%s", view)
	}
	if strings.Contains(view, "101%") {
		t.Fatalf("a percentage past the window is less useful than the gap:\n%s", view)
	}
}

func TestFooterSaysCompactingWhileItRuns(t *testing.T) {
	fa := &fakeAgent{ctxInfo: agent.ContextInfo{
		Used: 105000, Limit: 128000, Exact: true, Compacting: true,
		AutoCompact: true, CompactAt: 80,
	}}
	m, _ := newTestModel(t, fa)
	m.running = true
	if view := m.View(); !strings.Contains(view, "compacting…") {
		t.Fatalf("the status should say compaction is running:\n%s", view)
	}
}

func TestContextReportShowsTheLastRequest(t *testing.T) {
	fa := &fakeAgent{ctxInfo: agent.ContextInfo{
		Used: 12400, Limit: 128000, Exact: true, Source: agent.LimitFromTable,
		System: 1200, Tools: 800, Recent: 10000, RecentMessages: 6,
		AutoCompact: true, CompactAt: 80, KeepTurns: 3,
		LastInput: 12398, LastOutput: 214,
	}}
	m, _ := newTestModel(t, fa)
	m, _ = typeAndEnter(t, m, "/context")
	view := m.View()
	if !strings.Contains(view, "last request") || !strings.Contains(view, "12.4k input + 214 output") {
		t.Fatalf("/context should show what the provider reported:\n%s", view)
	}
	if strings.Contains(view, "(estimated)") {
		t.Fatalf("a measured figure should not be called estimated:\n%s", view)
	}
}

func TestCompactNotificationSaysToolResultsWereShortened(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	info := agent.CompactionInfo{Messages: 18, Before: 105000, After: 24000, Shortened: 2}
	var tm tea.Model = m
	tm, _ = tm.Update(eventMsg(agent.Event{Type: agent.EventCompaction, Compaction: &info}))
	m = tm.(*Model)
	if view := m.View(); !strings.Contains(view, "2 tool results shortened") {
		t.Fatalf("the block should say the tail was shortened:\n%s", view)
	}
}

func TestReplayShowsTheSummaryInsteadOfTheCompactedMessages(t *testing.T) {
	fa := &fakeAgent{
		summary:    "we fixed the parser",
		summarized: 4,
	}
	fa.setHistory(
		agent.NewUserMessage("the recent question"),
		agent.Message{Role: agent.RoleAssistant, Content: "the recent answer"},
	)
	m, _ := newTestModel(t, fa)
	m.replayHistory()
	m.layout()
	m.refresh()
	view := m.View()
	if !strings.Contains(view, "summary of earlier conversation") || !strings.Contains(view, "we fixed the parser") {
		t.Fatalf("the rebuilt transcript should show the summary:\n%s", view)
	}
	if !strings.Contains(view, "4 earlier messages") {
		t.Fatalf("it should say how many messages are not replayed:\n%s", view)
	}
	if !strings.Contains(view, "the recent question") {
		t.Fatalf("the kept messages should still be replayed:\n%s", view)
	}
}

func TestContextCommandDuringARunDoesNotReadTheAgent(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	m.running = true
	m.ctx = agent.ContextInfo{Used: 9000, Limit: 128000, Exact: true}
	fa.ctxInfo = agent.ContextInfo{Used: 1, Limit: 2} // would be wrong if read
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/context")})
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = tm.(*Model)
	if !strings.Contains(m.View(), "in flight") {
		t.Fatalf("/context should defer to the footer while a run is in flight:\n%s", m.View())
	}
	if m.ctx.Used != 9000 {
		t.Fatalf("the console's own figure should survive: %+v", m.ctx)
	}
}

func TestContextSummaryIsShownWhileARunIsInFlight(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	info := agent.ContextInfo{Used: 100, Limit: 128000, SummaryText: "the streamed summary"}
	var tm tea.Model = m
	tm, _ = tm.Update(eventMsg(agent.Event{Type: agent.EventContext, Context: &info}))
	m = tm.(*Model)
	m.running = true // the agent must not be read now
	m, _ = typeAndEnter(t, m, "/context summary")
	if !strings.Contains(m.View(), "the streamed summary") {
		t.Fatalf("/context summary should use the event stream while a run is in flight:\n%s", m.View())
	}
}

// TestOpeningADirectoryRecomputesTheIndicator covers `/new <dir>`: the console
// shows another directory's conversation, and the indicator follows it.
func TestOpeningADirectoryRecomputesTheIndicator(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	m.opts.OpenSession = func(dir string) (string, error) {
		if dir != "/work/other" {
			t.Fatalf("OpenSession got %q", dir)
		}
		fa.setHistory(agent.NewUserMessage("restored"))
		fa.ctxInfo = agent.ContextInfo{Used: 4321, Limit: 128000}
		return "(12 messages)", nil
	}
	m.openDirectory("/work/other")
	if m.ctx.Used != 4321 || m.ctx.Limit != 128000 {
		t.Fatalf("the indicator should follow the restored context: %+v", m.ctx)
	}
	if view := m.View(); !strings.Contains(view, "restored") {
		t.Fatalf("the restored conversation was not replayed:\n%s", view)
	}
}

// TestContextSeverityAppliesWithAutomaticCompactionOff pins §3.2: the bands
// describe how full the window is, not whether anything will act on it.
func TestContextSeverityAppliesWithAutomaticCompactionOff(t *testing.T) {
	c := agent.ContextInfo{Used: 110000, Limit: 128000, AutoCompact: false, CompactAt: 80}
	if got := contextColor(c); got != "203" {
		t.Fatalf("at the threshold the segment is critical even with compaction off, got %s", got)
	}
	if got := contextColor(agent.ContextInfo{Used: 90000, Limit: 128000, CompactAt: 80}); got != "214" {
		t.Fatalf("between the warning band and the threshold, got %s", got)
	}
}

// TestCompactionBlockReportsTheKeptTurns pins that the block says how many
// turns were actually kept.
func TestCompactionBlockReportsTheKeptTurns(t *testing.T) {
	fa := &fakeAgent{}
	m, _ := newTestModel(t, fa)
	var tm tea.Model = m
	info := agent.CompactionInfo{Messages: 9, Before: 105000, After: 24000, KeptTurns: 2, Automatic: true}
	tm, _ = tm.Update(eventMsg(agent.Event{Type: agent.EventCompaction, Compaction: &info}))
	m = tm.(*Model)
	if view := m.View(); !strings.Contains(view, "the last 2 turns kept") {
		t.Fatalf("the block should report the turns it kept:\n%s", view)
	}
}

// TestContextSummaryNeedsOnlyTheFirstWord pins that the two consoles agree:
// /context treats any argument word as the subcommand, so extra text does not
// turn it into a breakdown request.
func TestContextSummaryNeedsOnlyTheFirstWord(t *testing.T) {
	fa := &fakeAgent{ctxInfo: agent.ContextInfo{Used: 10, Limit: 100, SummaryText: "we changed the parser"}}
	m, _ := newTestModel(t, fa)
	var tm tea.Model = m
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/context summary please")})
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if view := tm.(*Model).View(); !strings.Contains(view, "we changed the parser") {
		t.Fatalf("/context summary with trailing words should print the summary:\n%s", view)
	}
}
