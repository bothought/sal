package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeProvider is a provider with a fixed answer, a context-window answer, and
// a record of the requests it was sent.
type fakeProvider struct {
	answer string
	model  string
	window int
	hasWin bool
	calls  int
	sent   [][]Message
}

func (p *fakeProvider) Name() string { return "fake" }

func (p *fakeProvider) ModelName() string {
	if p.model != "" {
		return p.model
	}
	return "fake-model"
}

func (p *fakeProvider) Stream(_ context.Context, msgs []Message, _ []ToolSpec, onText func(string)) (Message, error) {
	p.calls++
	p.sent = append(p.sent, append([]Message(nil), msgs...))
	if onText != nil {
		onText(p.answer)
	}
	return Message{Role: RoleAssistant, Content: p.answer}, nil
}

func (p *fakeProvider) ContextWindow(context.Context) (int, bool) {
	if !p.hasWin {
		return 0, false
	}
	return p.window, true
}

func testHistory(turns int) []Message {
	var msgs []Message
	for i := 0; i < turns; i++ {
		msgs = append(msgs, Message{Role: RoleUser, Content: strings.Repeat("u", 40)})
		msgs = append(msgs,
			Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "read", Arguments: "{}"}}},
			Message{Role: RoleTool, Content: strings.Repeat("t", 40), ToolCallID: "c", ToolName: "read"})
		msgs = append(msgs, Message{Role: RoleAssistant, Content: strings.Repeat("a", 40)})
	}
	return msgs
}

func TestEstimateTokensHandlesDenseScripts(t *testing.T) {
	ascii := EstimateTokens(strings.Repeat("a", 400))
	if ascii < 95 || ascii > 105 {
		t.Fatalf("400 ASCII chars should be ~100 tokens, got %d", ascii)
	}
	cjk := EstimateTokens(strings.Repeat("汉", 100))
	if cjk < 100 {
		t.Fatalf("100 CJK characters should not collapse to %d tokens", cjk)
	}
	if EstimateTokens("") != 0 {
		t.Fatal("empty text has no tokens")
	}
}

func TestLimitResolutionPrecedence(t *testing.T) {
	p := &fakeProvider{window: 32000, hasWin: true}
	a := New(p, nil, "/w", "")
	a.ContextWindowOverride = 64000
	if got, src := a.ContextWindow(); got != 64000 || src != LimitFromConfig {
		t.Fatalf("config should win: %d %s", got, src)
	}

	a.ContextWindowOverride = 0
	if got, src := a.resolveLimit(context.Background()); got != 32000 || src != LimitFromProvider {
		t.Fatalf("the endpoint should win over the table: %d %s", got, src)
	}

	// A model the table knows, from a provider that publishes nothing.
	a2 := New(&fakeProvider{answer: "x", model: "gpt-4o-mini"}, nil, "/w", "")
	if got, src := a2.resolveLimit(context.Background()); got != 128000 || src != LimitFromTable {
		t.Fatalf("the table should resolve gpt-4o: %d %s", got, src)
	}
	// A model nothing knows.
	a3 := New(&fakeProvider{answer: "x", model: "unknown-model"}, nil, "/w", "")
	if got, src := a3.resolveLimit(context.Background()); got != DefaultContextWindow || src != LimitFromDefault {
		t.Fatalf("an unknown model should fall back to the default: %d %s", got, src)
	}
	if n, ok := windowFromTable("gpt-4"); !ok || n != 8192 {
		t.Fatalf("gpt-4 must not match the gpt-4o entry: %d %v", n, ok)
	}
}

func TestModelSwitchInvalidatesTheCachedLimit(t *testing.T) {
	p := &switchingProvider{fakeProvider: fakeProvider{window: 8000, hasWin: true}}
	a := New(p, nil, "/w", "")
	if got, src := a.resolveLimit(context.Background()); got != 8000 || src != LimitFromProvider {
		t.Fatalf("limit = %d %s", got, src)
	}
	if !a.SetModelName("other-model") {
		t.Fatal("the switching provider should accept a new model")
	}
	p.hasWin = false
	p.model = "other-model"
	if got, src := a.ContextWindow(); got != DefaultContextWindow || src != LimitFromDefault {
		t.Fatalf("a model switch should re-resolve the window: %d %s", got, src)
	}
}

// switchingProvider is a fakeProvider that supports SetModelName.
type switchingProvider struct{ fakeProvider }

func (p *switchingProvider) SetModelName(m string) { p.model = m }

func TestUsedTokensIsMeasuredThenEstimated(t *testing.T) {
	p := &fakeProvider{answer: "hi"}
	a := New(p, nil, "/w", "")
	a.History = testHistory(2)

	if _, exact := a.UsedTokens(); exact {
		t.Fatal("nothing has been measured yet")
	}

	a.recordUsage(&Usage{InputTokens: 1000, OutputTokens: 20})
	used, exact := a.UsedTokens()
	if !exact || used != 1020 {
		t.Fatalf("measured figure should be exact: %d %v", used, exact)
	}

	// Appending after the measurement keeps the base and estimates the rest.
	// The figure is then an estimate again: part of it was not measured.
	a.History = append(a.History, Message{Role: RoleUser, Content: strings.Repeat("x", 400)})
	used, exact = a.UsedTokens()
	if exact {
		t.Fatal("a figure with an estimated part must not be reported as exact")
	}
	if used <= 1020 || used > 1020+110 {
		t.Fatalf("the appended message should be estimated, got %d", used)
	}

	a.invalidateContext()
	if _, exact := a.UsedTokens(); exact {
		t.Fatal("invalidation should drop the measured figure")
	}
}

func TestCompactKeepsTheRecentTurnsAndSummarizesTheRest(t *testing.T) {
	p := &fakeProvider{answer: "the summary"}
	a := New(p, nil, "/w", "")
	a.History = testHistory(6)
	a.ContextWindowOverride = 100000

	info, err := a.Compact(context.Background(), "", false)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if info.Messages != 12 {
		t.Fatalf("should summarize three of six turns (12 messages), got %d", info.Messages)
	}
	if a.Summary() != "the summary" {
		t.Fatalf("summary not kept: %q", a.Summary())
	}

	// 6 turns of 4 messages; 3 summarized, 3 kept verbatim behind the summary.
	ctx := a.contextMessages()
	if len(ctx) != 1+12 {
		t.Fatalf("active context = %d messages, want the summary plus 3 turns", len(ctx))
	}
	if !strings.Contains(ctx[0].Content, "summary of the conversation so far") {
		t.Fatalf("the summary must be labelled: %q", ctx[0].Content)
	}
	if ctx[0].Role != RoleUser {
		t.Fatalf("the summary takes the user role so the sequence stays valid: %q", ctx[0].Role)
	}
	if strings.Contains(ctx[0].Content, "something invented") {
		t.Fatal("the summary must be the model's answer, not the instruction")
	}
}

func TestCompactNeverSplitsAToolExchange(t *testing.T) {
	a := New(&fakeProvider{answer: "s"}, nil, "/w", "")
	a.ContextWindowOverride = 100000
	a.History = testHistory(4)
	a.KeepTurns = 2

	cut := a.compactionKeepCount()
	if cut <= 0 || cut >= len(a.History) {
		t.Fatalf("cut = %d", cut)
	}
	// The message the summary stops before must not be a tool result whose call
	// it summarizes, and the first kept message must be a user turn.
	if a.History[cut].Role != RoleUser {
		t.Fatalf("the kept part should start at a user turn, got %q", a.History[cut].Role)
	}
	info, err := a.Compact(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Messages != cut {
		t.Fatalf("compaction summarized %d, cut was %d", info.Messages, cut)
	}
}

func TestCompactWithNothingToSummarize(t *testing.T) {
	a := New(&fakeProvider{answer: "s"}, nil, "/w", "")
	a.History = testHistory(1)
	if _, err := a.Compact(context.Background(), "", false); !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("one turn is all tail, nothing to summarize: %v", err)
	}
	if a.Summary() != "" {
		t.Fatal("a failed compaction must leave the context alone")
	}
}

func TestCompactionFailureLeavesTheContextUnchanged(t *testing.T) {
	p := &scriptedProvider{script: []error{errors.New("boom")}, answer: "x"}
	a := New(p, nil, "/w", "")
	a.MaxRetries = -1
	a.History = testHistory(6)

	before := len(a.contextMessages())
	_, err := a.Compact(context.Background(), "", false)
	if err == nil {
		t.Fatal("expected the failure")
	}
	if a.Summary() != "" || len(a.contextMessages()) != before {
		t.Fatal("the context must be untouched when summarization fails")
	}
}

func TestAutoCompactionRunsWhenTheWindowFills(t *testing.T) {
	p := &fakeProvider{answer: "s"}
	a := New(p, nil, "/w", "")
	a.ContextWindowOverride = 100
	a.CompactAt = 10
	a.History = testHistory(6)

	var events []Event
	emit := func(ev Event) { events = append(events, ev) }
	a.compactForContext(context.Background(), emit)

	if a.Summary() == "" {
		t.Fatal("the context is over the threshold, so it should have compacted")
	}
	var sawCompaction, sawContext bool
	for _, ev := range events {
		switch ev.Type {
		case EventCompaction:
			sawCompaction = ev.Compaction != nil && ev.Compaction.Automatic
		case EventContext:
			sawContext = ev.Context != nil
		}
	}
	if !sawCompaction || !sawContext {
		t.Fatalf("compaction and a context update should be reported: %+v", events)
	}
}

func TestAutoCompactionRunsOnceThenDisablesItself(t *testing.T) {
	p := &fakeProvider{answer: "s"}
	a := New(p, nil, "/w", "")
	a.ContextWindowOverride = 10 // far smaller than the prompt and tools
	a.CompactAt = 1
	a.History = testHistory(6)

	a.compactForContext(context.Background(), func(Event) {})
	if !a.autoDisabled {
		t.Fatal("compaction that cannot free enough of the window should disable itself")
	}
	calls := p.calls
	a.compactForContext(context.Background(), func(Event) {})
	if p.calls != calls {
		t.Fatalf("a disabled compaction must not ask the model again: %d -> %d", calls, p.calls)
	}
}

func TestAutoCompactionCanBeDisabled(t *testing.T) {
	p := &fakeProvider{answer: "s"}
	a := New(p, nil, "/w", "")
	a.AutoCompact = false
	a.ContextWindowOverride = 100
	a.History = testHistory(6)

	a.compactForContext(context.Background(), func(Event) {})
	if a.Summary() != "" || p.calls != 0 {
		t.Fatal("--no-auto-compact must not summarize")
	}
}

func TestRequestsCarryTheSummaryInsteadOfTheCompactedMessages(t *testing.T) {
	p := &fakeProvider{answer: "s"}
	a := New(p, nil, "/w", "")
	a.ContextWindowOverride = 100000
	a.History = testHistory(6)
	if _, err := a.Compact(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}

	sent := a.requestMessages()
	if strings.Contains(sent[1].Content, strings.Repeat("u", 40)) {
		t.Fatal("a summarized message must not be sent again")
	}
	if !strings.Contains(sent[1].Content, "s") {
		t.Fatal("the summary should be in the request")
	}
	if len(sent) != 1+1+12 {
		t.Fatalf("request = %d messages, want system + summary + 3 turns (15)", len(sent))
	}
}

func TestSetHistoryAndResetDropTheCompaction(t *testing.T) {
	a := New(&fakeProvider{answer: "s"}, nil, "/w", "")
	a.ContextWindowOverride = 100000
	a.History = testHistory(6)
	if _, err := a.Compact(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	a.SetHistory(testHistory(2))
	if a.Summary() != "" {
		t.Fatal("switching conversations must drop the summary")
	}
	a.SetCompaction(&Compacted{Summary: "restored", Through: 1, Messages: 2})
	if a.Summary() != "restored" {
		t.Fatal("a resumed compaction should be restorable")
	}
	a.Reset()
	if a.Summary() != "" {
		t.Fatal("/new must drop the summary")
	}
}

func TestShorteningFitsTheTarget(t *testing.T) {
	a := New(&fakeProvider{answer: "s"}, nil, "/w", "")
	a.ContextWindowOverride = 200
	a.CompactAt = 10
	a.History = testHistory(6)
	// Make the kept tail dominate: big tool results in the last turns.
	for i := len(a.History) - 12; i < len(a.History); i++ {
		if a.History[i].Role == RoleTool {
			a.History[i].Content = strings.Repeat("t", 2000)
		}
	}
	info, err := a.Compact(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Shortened == 0 {
		t.Fatal("the tail is far larger than the target, so tool results should be shortened")
	}
	found := false
	for _, m := range a.contextMessages() {
		if m.Content == omittedMarker {
			found = true
		}
	}
	if !found {
		t.Fatal("a shortened tool result should be sent as the marker")
	}
}

func TestContextInfoReportsTheBreakdown(t *testing.T) {
	a := New(&fakeProvider{answer: "s"}, nil, "/w", "")
	a.History = testHistory(2)
	info := a.ContextInfo()
	if info.Limit != DefaultContextWindow {
		t.Fatalf("limit = %d", info.Limit)
	}
	if info.System == 0 {
		t.Fatal("the system prompt should be accounted for")
	}
	if info.Used == 0 || info.Used < info.System {
		t.Fatalf("used = %d, system = %d", info.Used, info.System)
	}
	if info.Exact {
		t.Fatal("nothing was measured, so the figure is an estimate")
	}
	if !info.AutoCompact || info.CompactAt != DefaultCompactAt || info.KeepTurns != DefaultKeepTurns {
		t.Fatalf("policy should be reported: %+v", info)
	}
}

// TestRepeatedCompactionSubsumesThePreviousSummary pins the rule that a second
// compaction produces one summary that contains what the first one recorded,
// rather than summarizing only the messages added since and dropping the rest.
func TestRepeatedCompactionSubsumesThePreviousSummary(t *testing.T) {
	p := &fakeProvider{answer: "first summary"}
	a := New(p, nil, "/w", "")
	a.ContextWindowOverride = 100000
	a.History = testHistory(6)

	if _, err := a.Compact(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	firstMessages := a.compaction.Messages
	if firstMessages == 0 {
		t.Fatal("the first compaction should represent some messages")
	}

	// Add more turns, then compact again.
	a.History = append(a.History, testHistory(4)...)
	p.answer = "second summary"
	info, err := a.Compact(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Messages <= firstMessages {
		t.Fatalf("the second summary should subsume the first: %d messages, was %d", info.Messages, firstMessages)
	}
	if a.compaction.Messages != info.Messages {
		t.Fatalf("the recorded count should match the report: %d vs %d", a.compaction.Messages, info.Messages)
	}
	// The request that produced the second summary must carry the first one,
	// which is what makes the new summary subsume it.
	last := p.sent[len(p.sent)-1]
	if len(last) == 0 || !strings.Contains(last[0].Content, "first summary") {
		t.Fatalf("the previous summary should be carried into the next summary request: %+v", last)
	}
	if !strings.Contains(a.Summary(), "second summary") {
		t.Fatalf("the active summary = %q", a.Summary())
	}
}

// TestActiveContextIsTheSummaryAndTheTail checks what a transcript rebuilt
// after a resume replays: the summary and the messages after it, not the
// messages it replaced.
func TestActiveContextIsTheSummaryAndTheTail(t *testing.T) {
	a := New(&fakeProvider{answer: "the summary"}, nil, "/w", "")
	a.ContextWindowOverride = 100000
	a.History = testHistory(6)

	full := len(a.History)
	if _, err := a.Compact(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	summary, msgs, summarized := a.ActiveContext()
	if summary != "the summary" {
		t.Fatalf("summary = %q", summary)
	}
	if summarized == 0 || len(msgs) != full-summarized {
		t.Fatalf("active context: %d summarized, %d messages, history had %d", summarized, len(msgs), full)
	}
	if len(a.Messages()) != full {
		t.Fatal("the history itself must stay complete")
	}

	// Without a compaction the active context is the whole conversation.
	b := New(&fakeProvider{}, nil, "/w", "")
	b.History = testHistory(2)
	if s, m, n := b.ActiveContext(); s != "" || n != 0 || len(m) != len(b.History) {
		t.Fatalf("uncompacted active context = %q, %d messages, %d summarized", s, len(m), n)
	}
}

// TestRunReportsTheContextBeforeTheFirstRequest pins the event an interface
// needs to show a figure in a conversation that has never been measured.
func TestRunReportsTheContextBeforeTheFirstRequest(t *testing.T) {
	p := &fakeProvider{answer: "hi"}
	a := New(p, nil, "/w", "")
	a.History = testHistory(1)
	events := 0
	var first string
	_ = a.RunPrompt(context.Background(), "", func(ev Event) {
		if events == 0 {
			first = ev.Type
		}
		events++
	})
	if events == 0 || first != EventContext {
		t.Fatalf("the first event should report the context, got %q", first)
	}
}

// TestLimitResolutionDoesNotCacheAGuess pins the precedence the feature
// document promises: an interface drawing its footer resolves the window
// without asking the endpoint, and that must not stop the first run from
// asking. A table or default answer is a guess and is not remembered.
func TestLimitResolutionDoesNotCacheAGuess(t *testing.T) {
	p := &fakeProvider{model: "gpt-4o-mini", window: 32000, hasWin: true}
	a := New(p, nil, "/w", "")

	// The footer asks without a context; the table's answer is provisional.
	if got, src := a.ContextWindow(); got != 128000 || src != LimitFromTable {
		t.Fatalf("footer resolution = %d %s", got, src)
	}
	// The first request can reach the endpoint, so its answer must win.
	if got, src := a.resolveLimit(context.Background()); got != 32000 || src != LimitFromProvider {
		t.Fatalf("the endpoint should still be asked: %d %s", got, src)
	}
	// And the answer is remembered.
	if got, src := a.ContextWindow(); got != 32000 || src != LimitFromProvider {
		t.Fatalf("the endpoint's answer should be cached: %d %s", got, src)
	}
}

// TestExactnessDropsWhenAnythingIsAppended pins §3.4: a figure is exact only
// when no part of it is estimated.
func TestExactnessDropsWhenAnythingIsAppended(t *testing.T) {
	a := New(&fakeProvider{answer: "x"}, nil, "/w", "")
	a.History = testHistory(1)
	a.recordUsage(&Usage{InputTokens: 1000, OutputTokens: 20})
	if _, exact := a.UsedTokens(); !exact {
		t.Fatal("a freshly measured context is exact")
	}
	a.History = append(a.History, NewUserMessage("one more thing"))
	if _, exact := a.UsedTokens(); exact {
		t.Fatal("a figure with an estimated part is not exact")
	}
}

// TestCompactionReportsTheTurnsItKept pins the notification's figures: the
// count is what was kept, not the default.
func TestCompactionReportsTheTurnsItKept(t *testing.T) {
	a := New(&fakeProvider{answer: "s"}, nil, "/w", "")
	a.ContextWindowOverride = 100000
	a.KeepTurns = 2
	a.History = testHistory(5)
	info, err := a.Compact(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.KeptTurns != 2 {
		t.Fatalf("kept turns = %d, want the configured 2", info.KeptTurns)
	}
}

// TestAutoCompactionLeavesTheRunAloneWhenAborted pins §4.7: cancelling a
// summarization reports a cancellation, and the run is not failed by it.
func TestAutoCompactionLeavesTheRunAloneWhenAborted(t *testing.T) {
	p := &blockingProvider{started: make(chan struct{})}
	a := New(p, nil, "/w", "")
	a.ContextWindowOverride = 10
	a.CompactAt = 1
	a.History = testHistory(6)

	ctx, cancel := context.WithCancel(context.Background())
	var events []Event
	go func() {
		<-p.started
		cancel()
	}()
	a.compactForContext(ctx, func(ev Event) { events = append(events, ev) })

	var note string
	for _, ev := range events {
		if ev.Type == EventCompaction && ev.Compaction != nil {
			note = ev.Compaction.Note
		}
	}
	if !strings.Contains(note, "cancelled") {
		t.Fatalf("a cancelled compaction should say so: %+v", events)
	}
	if a.Summary() != "" {
		t.Fatal("a cancelled compaction must leave the context unchanged")
	}
}

// blockingProvider blocks in Stream until its context is cancelled.
type blockingProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Name() string      { return "blocking" }
func (p *blockingProvider) ModelName() string { return "blocking-model" }
func (p *blockingProvider) Stream(ctx context.Context, _ []Message, _ []ToolSpec, _ func(string)) (Message, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return Message{}, ctx.Err()
}

// TestChunksNeverSplitAToolExchange pins the chunking path: when the messages
// being summarized do not fit in one request, every chunk still starts at a
// message the conversation allows, so a tool result is never separated from the
// call that produced it.
func TestChunksNeverSplitAToolExchange(t *testing.T) {
	p := &fakeProvider{answer: "s"}
	a := New(p, nil, "/w", "")
	a.ContextWindowOverride = 100000
	a.History = testHistory(6)

	// A budget far too small for the conversation forces several chunks.
	prior := ""
	chunks := 0
	for from := 0; from < len(a.History); {
		to := a.fitChunk(from, len(a.History), prior, 0, 40)
		to = a.chunkBoundary(from, to, len(a.History))
		if to == from {
			to = a.nextBoundary(from, len(a.History))
		}
		if a.History[from].Role == RoleTool {
			t.Fatalf("chunk %d starts with a tool result, whose call is in the previous chunk", chunks)
		}
		if to < len(a.History) && len(a.History[to-1].ToolCalls) > 0 {
			t.Fatalf("chunk %d ends on a tool call without its results", chunks)
		}
		chunks++
		from = to
		if chunks > len(a.History) {
			t.Fatal("chunking did not make progress")
		}
	}
	if chunks < 2 {
		t.Fatalf("the budget should have forced several chunks, got %d", chunks)
	}

	// The real path produces one summary from all of them.
	if _, err := a.summarizeFrom(context.Background(), 0, len(a.History), "", "", 40); err != nil {
		t.Fatal(err)
	}
	if p.calls < 2 {
		t.Fatalf("the summarization should have been chunked, got %d calls", p.calls)
	}
}

// TestRunResolvesTheWindowWithAutoCompactOff pins where the window gets
// resolved: the compaction check returns early when automatic compaction is
// off, so a run must ask the endpoint itself, or every figure it reports for
// the conversation stays the built-in table's guess.
func TestRunResolvesTheWindowWithAutoCompactOff(t *testing.T) {
	p := &fakeProvider{answer: "hi", model: "deepseek-v4.1-flash", window: 1000000, hasWin: true}
	a := New(p, nil, "/w", "")
	a.AutoCompact = false
	a.History = testHistory(1)

	var limits []int
	var sources []string
	if err := a.RunPrompt(context.Background(), "", func(ev Event) {
		if ev.Type == EventContext && ev.Context != nil {
			limits = append(limits, ev.Context.Limit)
			sources = append(sources, ev.Context.Source)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(limits) == 0 {
		t.Fatal("the run reported no context")
	}
	for i := range limits {
		if limits[i] != 1000000 || sources[i] != LimitFromProvider {
			t.Fatalf("context %d = %d %s, want the endpoint's 1000000", i, limits[i], sources[i])
		}
	}
}
