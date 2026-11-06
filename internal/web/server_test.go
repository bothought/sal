package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bothought/sal/internal/agent"
	"github.com/bothought/sal/internal/session"
)

// fakeAgent is a scripted stand-in for *agent.Agent.
type fakeAgent struct {
	mu         sync.Mutex
	model      string
	script     []agent.Event
	blocking   bool
	failWith   error
	prompts    []string
	aborted    bool
	resets     int
	history    []agent.Message
	ctxInfo    agent.ContextInfo
	summary    string
	summarized int
	compacts   []string
	// release unblocks a blocking run without cancelling it.
	release chan struct{}
}

func (f *fakeAgent) RunPrompt(ctx context.Context, prompt string, emit func(agent.Event)) error {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	script := append([]agent.Event(nil), f.script...)
	blocking, failWith, release := f.blocking, f.failWith, f.release
	f.mu.Unlock()

	for _, ev := range script {
		emit(ev)
	}
	if blocking {
		if release != nil {
			// A run that can be let go without being cancelled.
			select {
			case <-release:
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				f.mu.Lock()
				f.aborted = true
				f.mu.Unlock()
				return ctx.Err()
			}
			return nil
		}
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
func (f *fakeAgent) Reset()                     { f.mu.Lock(); f.resets++; f.mu.Unlock() }

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
	f.mu.Unlock()
	return agent.CompactionInfo{Messages: 9, Before: 105000, After: 24000, Automatic: automatic}, nil
}
func (f *fakeAgent) ToolNames() []string { return []string{"read", "bash"} }

func (f *fakeAgent) Messages() []agent.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Message(nil), f.history...)
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

// sseStream reads server-sent events from a response body into a channel.
type sseStream struct {
	t      *testing.T
	events chan outEvent
	cancel func()
}

func connectSSE(t *testing.T, base, token string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/events?token="+token, nil)
	if err != nil {
		t.Fatalf("building SSE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("SSE connect: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("SSE connect status = %d", resp.StatusCode)
	}
	s := &sseStream{t: t, events: make(chan outEvent, 128), cancel: cancel}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev outEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				continue
			}
			select {
			case s.events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return s
}

// waitFor blocks until an event of the given type arrives, or the deadline
// passes. Interior events are discarded.
func (s *sseStream) waitFor(t *testing.T, kind string) outEvent {
	t.Helper()
	return s.waitForMatch(t, func(ev outEvent) bool { return ev.Type == kind }, kind)
}

// waitForMatch blocks until an event satisfying match arrives.
func (s *sseStream) waitForMatch(t *testing.T, match func(outEvent) bool, what string) outEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var seen []string
	for {
		select {
		case ev := <-s.events:
			if match(ev) {
				return ev
			}
			seen = append(seen, ev.Type)
		case <-deadline:
			t.Fatalf("timed out waiting for %s (saw %v)", what, seen)
		}
	}
}

// waitFor blocks until cond is true, or the deadline passes.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (s *sseStream) close() { s.cancel() }

func newTestServer(t *testing.T, a Agent, opts ...func(*Options)) *Server {
	t.Helper()
	o := Options{Agent: a, Cwd: "/work/project", Version: "test"}
	for _, fn := range opts {
		fn(&o)
	}
	return New(o)
}

func post(t *testing.T, base, token, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sal-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func TestPromptStreamsEventsAndTranscript(t *testing.T) {
	fake := &fakeAgent{script: []agent.Event{
		{Type: agent.EventTextDelta, Text: "Hello "},
		{Type: agent.EventTextDelta, Text: "world"},
		{Type: agent.EventDone},
	}}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	snapshot := stream.waitFor(t, "state")
	if len(snapshot.Blocks) != 0 {
		t.Fatalf("fresh console should have an empty transcript, got %+v", snapshot.Blocks)
	}

	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"say hi"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("prompt status = %d", resp.StatusCode)
	}

	user := stream.waitFor(t, "user")
	if user.Text != "say hi" {
		t.Fatalf("user event text = %q", user.Text)
	}
	delta := stream.waitFor(t, "delta")
	if delta.Text != "Hello " {
		t.Fatalf("first delta = %q", delta.Text)
	}
	if got := stream.waitFor(t, "delta").Text; got != "world" {
		t.Fatalf("second delta = %q", got)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-stream.events:
			if ev.Type == "status" && !ev.Running {
				goto settled
			}
		case <-deadline:
			t.Fatal("run never settled")
		}
	}
settled:

	snap := srv.snapshotEvent()
	if snap.Running {
		t.Fatal("console should be idle after the run")
	}
	// user block + assistant block
	if len(snap.Blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d: %+v", len(snap.Blocks), snap.Blocks)
	}
	if snap.Blocks[0].Kind != kindUser || snap.Blocks[0].Body != "say hi" {
		t.Fatalf("unexpected user block: %+v", snap.Blocks[0])
	}
	if snap.Blocks[1].Kind != kindAssistant || snap.Blocks[1].Body != "Hello world" {
		t.Fatalf("unexpected assistant block: %+v", snap.Blocks[1])
	}
}

func TestToolEventsRenderIntoTranscript(t *testing.T) {
	call := agent.ToolCall{ID: "1", Name: "read", Arguments: `{"path":"main.go"}`}
	fake := &fakeAgent{script: []agent.Event{
		{Type: agent.EventToolStart, ToolCall: &call},
		{Type: agent.EventToolResult, Result: &agent.ToolResult{Name: "read", Content: "1| package main", DurationMS: 3}},
		{Type: agent.EventTurnEnd},
		{Type: agent.EventTextDelta, Text: "done"},
		{Type: agent.EventDone},
	}}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"read it"}`).Body.Close()

	toolEv := stream.waitFor(t, "tool")
	if !strings.Contains(toolEv.Title, "read") || !strings.Contains(toolEv.Title, "main.go") {
		t.Fatalf("tool title = %q", toolEv.Title)
	}
	res := stream.waitFor(t, "tool_result")
	if res.Body != "1| package main" || res.DurationMS != 3 {
		t.Fatalf("tool result = %+v", res)
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-stream.events:
			if ev.Type == "status" && !ev.Running {
				goto settled
			}
		case <-deadline:
			t.Fatal("run never settled")
		}
	}
settled:

	snap := srv.snapshotEvent()
	if len(snap.Blocks) != 3 {
		t.Fatalf("expected user+tool+assistant, got %+v", snap.Blocks)
	}
	if b := snap.Blocks[1]; b.Kind != kindTool || b.Body != "1| package main" || b.IsError {
		t.Fatalf("unexpected tool block: %+v", b)
	}
}

func TestToolResultErrorsAreMarked(t *testing.T) {
	call := agent.ToolCall{ID: "1", Name: "bash", Arguments: `{"command":"false"}`}
	fake := &fakeAgent{script: []agent.Event{
		{Type: agent.EventToolStart, ToolCall: &call},
		{Type: agent.EventToolResult, Result: &agent.ToolResult{Name: "bash", Content: "exit code 1", IsError: true}},
		{Type: agent.EventDone},
	}}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"run"}`).Body.Close()

	res := stream.waitFor(t, "tool_result")
	if !res.IsError {
		t.Fatal("failed tool result should be flagged")
	}
	waitIdle(t, stream)
	if b := srv.snapshotEvent().Blocks[1]; !b.IsError {
		t.Fatalf("tool block should be marked as an error: %+v", b)
	}
}

func waitIdle(t *testing.T, s *sseStream) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-s.events:
			if ev.Type == "status" && !ev.Running {
				return
			}
		case <-deadline:
			t.Fatal("run never settled")
		}
	}
}

func TestQueuedPromptRunsAfterCurrentTurn(t *testing.T) {
	fake := &fakeAgent{blocking: true}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"first"}`).Body.Close()
	stream.waitFor(t, "user")

	// The second prompt must be queued, not rejected or run concurrently.
	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"second"}`)
	var queued struct {
		Queued bool `json:"queued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		t.Fatalf("decoding queue response: %v", err)
	}
	resp.Body.Close()
	if !queued.Queued {
		t.Fatal("second prompt should have been queued")
	}
	if got := fake.promptCount(); got != 1 {
		t.Fatalf("agent should still be on the first prompt, got %d", got)
	}

	srv.Abort()
	stream.waitFor(t, "system") // "aborted by user"
	// The queued prompt starts as soon as the aborted run returns, which is when
	// the fake records the cancellation.
	stream.waitFor(t, "user")
	if !fake.wasAborted() {
		t.Fatal("abort did not cancel the run")
	}
	if got := fake.promptCount(); got != 2 {
		t.Fatalf("queued prompt should have started, got %d prompts", got)
	}
}

func TestAbortWhenIdleIsANoop(t *testing.T) {
	fake := &fakeAgent{}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp := post(t, ts.URL, srv.Token(), "/api/abort", `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("abort status = %d", resp.StatusCode)
	}
	if fake.wasAborted() {
		t.Fatal("nothing was running, so nothing should have been aborted")
	}
}

func TestAuthRejectsWrongOrMissingToken(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, tc := range []struct{ name, path string }{
		{"index", "/"},
		{"state", "/api/state"},
		{"prompt", "/api/prompt"},
	} {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s without a token = %d, want 403", tc.name, resp.StatusCode)
		}
	}

	resp, err := http.Get(ts.URL + "/?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token = %d, want 403", resp.StatusCode)
	}
}

func TestIndexServesTheConsoleWithToken(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/?token=" + srv.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<title>sal</title>") {
		t.Fatalf("index is not the console page:\n%s", body)
	}
}

func TestInitialPromptStartsARun(t *testing.T) {
	fake := &fakeAgent{script: []agent.Event{{Type: agent.EventTextDelta, Text: "hi"}, {Type: agent.EventDone}}}
	srv := newTestServer(t, fake, func(o *Options) { o.InitialPrompt = "hello there" })

	deadline := time.After(5 * time.Second)
	for srv.snapshotEvent().Running {
		select {
		case <-deadline:
			t.Fatal("initial prompt never ran")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := fake.promptCount(); got != 1 {
		t.Fatalf("initial prompt should have run once, got %d", got)
	}
	blocks := srv.snapshotEvent().Blocks
	if len(blocks) != 2 || blocks[0].Body != "hello there" || blocks[1].Body != "hi" {
		t.Fatalf("unexpected transcript: %+v", blocks)
	}
}

func TestEmptyPromptIsRejected(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"   "}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty prompt status = %d, want 400", resp.StatusCode)
	}
}

func TestNewSessionAndModelCommands(t *testing.T) {
	fake := &fakeAgent{}
	srv := newTestServer(t, fake, func(o *Options) {
		o.NewSession = func() (string, error) { return "(session abc)", nil }
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := post(t, ts.URL, srv.Token(), "/api/model", `{"model":"gpt-x"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model switch status = %d", resp.StatusCode)
	}
	if fake.ModelName() != "gpt-x" {
		t.Fatalf("model not switched: %q", fake.ModelName())
	}

	resp = post(t, ts.URL, srv.Token(), "/api/new", `{}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new session status = %d", resp.StatusCode)
	}
	if fake.resets != 1 {
		t.Fatalf("new session should reset the agent, resets = %d", fake.resets)
	}
	found := false
	for _, b := range srv.snapshotEvent().Blocks {
		if b.Title == "new session" && strings.Contains(b.Body, "(session abc)") {
			found = true
		}
	}
	if !found {
		t.Fatalf("new session should be noted in the transcript: %+v", srv.snapshotEvent().Blocks)
	}
}

func TestRunFailureIsReportedToThePage(t *testing.T) {
	fake := &fakeAgent{failWith: errors.New("boom")}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"go"}`).Body.Close()
	if ev := stream.waitFor(t, "error"); ev.Text != "boom" {
		t.Fatalf("error event = %+v", ev)
	}
	waitIdle(t, stream)
}

func TestURLRewritesWildcardHost(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{})
	for _, tc := range []struct{ addr, want string }{
		{"0.0.0.0:8080", "http://127.0.0.1:8080/"},
		{"[::]:8080", "http://127.0.0.1:8080/"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000/"},
		{":7000", "http://127.0.0.1:7000/"},
	} {
		got := srv.URL(tc.addr)
		if !strings.HasPrefix(got, tc.want) {
			t.Fatalf("URL(%q) = %q, want prefix %q", tc.addr, got, tc.want)
		}
		if !strings.HasSuffix(got, "?token="+srv.Token()) {
			t.Fatalf("URL(%q) = %q, want the access token appended", tc.addr, got)
		}
	}
}

// ---- token persistence and session ownership -------------------------------

func TestTokenFileIsCreatedAndReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-token")

	first := New(Options{Agent: &fakeAgent{}, TokenFile: path})
	if len(first.Token()) < 16 {
		t.Fatalf("token = %q, want a generated token", first.Token())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("token file was not written: %v", err)
	}
	// The mode is only meaningful where the platform keeps POSIX permissions;
	// Windows reports a fixed mode for every file, so asserting it there would
	// test the host and not the program.
	if perm, ok := posixFileMode(path); ok && perm != 0o600 {
		t.Fatalf("token file mode = %04o, want 0600", perm)
	}

	// a second process reuses the stored token, which is what keeps an already
	// open page working across a restart
	second := New(Options{Agent: &fakeAgent{}, TokenFile: path})
	if second.Token() != first.Token() {
		t.Fatalf("token was not reused: %q then %q", first.Token(), second.Token())
	}

	// an explicit token wins over the file
	third := New(Options{Agent: &fakeAgent{}, Token: "explicit-token", TokenFile: path})
	if third.Token() != "explicit-token" {
		t.Fatalf("explicit token = %q", third.Token())
	}

	// without a token file, every server gets a fresh token
	random := New(Options{Agent: &fakeAgent{}})
	if random.Token() == first.Token() {
		t.Fatal("expected an independent in-memory token")
	}
}

func TestURLCarriesTheSessionID(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, func(o *Options) {
		o.SessionID = func() string { return "abcdef1234567890" }
	})
	got := srv.URL("0.0.0.0:8080")
	want := "http://127.0.0.1:8080/?token=" + srv.Token() + "&session=abcdef1234567890"
	if got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestStoredConversationIsRestoredIntoTheTranscript(t *testing.T) {
	ag := &fakeAgent{}
	ag.setHistory(
		agent.NewUserMessage("list the go files"),
		agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
			{ID: "call-1", Name: "ls", Arguments: `{"path":"."}`},
		}},
		agent.Message{Role: agent.RoleTool, ToolCallID: "call-1", ToolName: "ls", Content: "main.go\nreadme.md"},
		agent.Message{Role: agent.RoleAssistant, Content: "Two files."},
		agent.Message{Role: agent.RoleUser, Content: "and the bash tool?"},
		agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
			{ID: "call-2", Name: "bash", Arguments: `{"command":"exit 1"}`},
		}},
		agent.Message{Role: agent.RoleTool, ToolCallID: "call-2", ToolName: "bash",
			Content: "Error: exit status 1"},
	)
	srv := newTestServer(t, ag)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	ev := stream.waitFor(t, "state")

	var kinds, titles []string
	for _, b := range ev.Blocks {
		kinds = append(kinds, b.Kind)
		titles = append(titles, b.Title)
	}
	want := []string{"user", "tool", "assistant", "user", "tool", "system"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("blocks = %v (%v), want %v", kinds, titles, want)
	}
	if body := ev.Blocks[0].Body; body != "list the go files" {
		t.Fatalf("user block = %q", body)
	}
	if body := ev.Blocks[1].Body; body != "main.go\nreadme.md" {
		t.Fatalf("tool result not folded into its call: %q", body)
	}
	if ev.Blocks[1].IsError {
		t.Fatal("a successful result must not be marked as an error")
	}
	if !ev.Blocks[4].IsError {
		t.Fatalf("a failed result must be marked as an error: %+v", ev.Blocks[4])
	}
	if !ev.Blocks[0].Done || !ev.Blocks[1].Done {
		t.Fatal("restored blocks must be complete, not streaming")
	}
}

func TestOpenSessionCallbackRestoresTheNamedSession(t *testing.T) {
	ag := &fakeAgent{}
	id := "aaaa000011112222"
	var opened string
	srv := newTestServer(t, ag, func(o *Options) {
		o.SessionID = func() string { return id }
		o.OpenSession = func(want string) (string, error) {
			opened = want
			ag.setHistory(agent.NewUserMessage("from " + want))
			id = want
			return want, nil
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/?token=" + srv.Token() + "&session=cccc666677778888")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if opened != "cccc666677778888" {
		t.Fatalf("opened = %q, want the session named in the URL", opened)
	}
	if id != "cccc666677778888" {
		t.Fatalf("server session = %q", id)
	}

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	state := stream.waitFor(t, "state")
	if state.SessionID != "cccc666677778888" {
		t.Fatalf("state sessionId = %q", state.SessionID)
	}
	if len(state.Blocks) == 0 || state.Blocks[0].Body != "from cccc666677778888" {
		t.Fatalf("transcript not restored from the opened session: %+v", state.Blocks)
	}
}

func TestUnknownSessionInTheURLLeavesTheConsoleUsable(t *testing.T) {
	var attempts int
	srv := newTestServer(t, &fakeAgent{}, func(o *Options) {
		o.SessionID = func() string { return "aaaa000011112222" }
		o.OpenSession = func(string) (string, error) {
			attempts++
			return "", errors.New("no such session")
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/?token=" + srv.Token() + "&session=missing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d, want a served page", resp.StatusCode)
	}
	if attempts != 1 {
		t.Fatalf("open attempts = %d", attempts)
	}
	if got := srv.SessionID(); got != "aaaa000011112222" {
		t.Fatalf("session changed to %q after a failed open", got)
	}
}

func TestSessionMenuIsSentWithTheSnapshot(t *testing.T) {
	now := time.Now()
	fake := &fakeAgent{}
	srv := newTestServer(t, fake, func(o *Options) {
		o.SessionID = func() string { return "aaaa000011112222" }
		o.ListSessions = func() []session.Info {
			return []session.Info{
				{ID: "bbbb111122223333", Name: "older task", Model: "gpt-x", Messages: 4, UpdatedAt: now},
				{ID: "aaaa000011112222", Name: "current", Model: "gpt-x", Messages: 2, UpdatedAt: now},
			}
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	state := stream.waitFor(t, "state")
	if len(state.Sessions) != 2 {
		t.Fatalf("sessions in the snapshot = %+v, want 2", state.Sessions)
	}
	if state.Sessions[0].ID != "bbbb111122223333" || state.Sessions[0].Name != "older task" ||
		state.Sessions[0].Messages != 4 || state.Sessions[0].UpdatedAt == "" {
		t.Fatalf("first session entry = %+v", state.Sessions[0])
	}
	if !state.Sessions[1].Current {
		t.Fatalf("the current session must be flagged: %+v", state.Sessions[1])
	}
	if state.Sessions[0].Current {
		t.Fatalf("only the current session may be flagged: %+v", state.Sessions[0])
	}
}

func TestOpenSessionEndpointSwitchesAndBroadcasts(t *testing.T) {
	ag := &fakeAgent{}
	id := "aaaa000011112222"
	srv := newTestServer(t, ag, func(o *Options) {
		o.SessionID = func() string { return id }
		o.OpenSession = func(want string) (string, error) {
			ag.setHistory(agent.NewUserMessage("from " + want))
			id = want
			return "(" + want + ")", nil
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	resp := post(t, ts.URL, srv.Token(), "/api/session/open", `{"id":"cccc666677778888"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open status = %d", resp.StatusCode)
	}
	if got := srv.SessionID(); got != "cccc666677778888" {
		t.Fatalf("server session = %q", got)
	}

	// The pages see the switch as a new snapshot, already holding the
	// conversation that was opened.
	state := stream.waitFor(t, "state")
	if state.SessionID != "cccc666677778888" {
		t.Fatalf("broadcast sessionId = %q", state.SessionID)
	}
	if len(state.Blocks) == 0 || state.Blocks[0].Body != "from cccc666677778888" {
		t.Fatalf("transcript not rebuilt for the opened session: %+v", state.Blocks)
	}
	var noted bool
	for _, b := range state.Blocks {
		if b.Body == "switched to session (cccc666677778888)" {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the switch should be noted in the transcript: %+v", state.Blocks)
	}
}

func TestOpenSessionEndpointReportsFailure(t *testing.T) {
	id := "aaaa000011112222"
	srv := newTestServer(t, &fakeAgent{}, func(o *Options) {
		o.SessionID = func() string { return id }
		o.OpenSession = func(string) (string, error) { return "", errors.New("no such session") }
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := post(t, ts.URL, srv.Token(), "/api/session/open", `{"id":"missing"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("open status = %d, want 409", resp.StatusCode)
	}
	if got := srv.SessionID(); got != id {
		t.Fatalf("session changed to %q after a failed open", got)
	}
}

func TestSessionEndpointsRequireToken(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, func(o *Options) {
		o.SessionID = func() string { return "aaaa000011112222" }
		o.OpenSession = func(string) (string, error) { return "", nil }
		o.ListSessions = func() []session.Info { return nil }
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/api/sessions", "/api/session/open"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s without a token = %d, want 403", path, resp.StatusCode)
		}
	}
}

func TestSessionsEndpointListsWithoutTheCurrentOneFlagged(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, func(o *Options) {
		o.SessionID = func() string { return "aaaa000011112222" }
		o.ListSessions = func() []session.Info {
			return []session.Info{{ID: "aaaa000011112222", Messages: 1}}
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := post(t, ts.URL, srv.Token(), "/api/sessions", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sessions status = %d", resp.StatusCode)
	}
	var body struct {
		Sessions []sessionInfo `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 || !body.Sessions[0].Current {
		t.Fatalf("sessions = %+v", body.Sessions)
	}
}

func TestOpenSessionWithNoOpenerIsRejected(t *testing.T) {
	id := "aaaa000011112222"
	srv := newTestServer(t, &fakeAgent{}, func(o *Options) {
		o.SessionID = func() string { return id }
	})
	if err := srv.OpenSession("bbbb111122223333"); err == nil {
		t.Fatal("expected an error when sessions are disabled")
	}
	if err := srv.OpenSession(id); err != nil {
		t.Fatalf("opening the current session should be a no-op: %v", err)
	}
}

func TestRetryIsReportedToThePage(t *testing.T) {
	fake := &fakeAgent{script: []agent.Event{
		{Type: agent.EventRetry, Error: "read: connection reset by peer",
			Attempt: 1, MaxAttempts: 4, RetryInMS: 500},
		{Type: agent.EventTextDelta, Text: "recovered"},
		{Type: agent.EventDone},
	}}
	srv := newTestServer(t, fake)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"go"}`)
	resp.Body.Close()

	ev := stream.waitFor(t, "system")
	if ev.Title != "retrying" {
		t.Fatalf("expected a retrying notice, got %+v", ev)
	}
	if !strings.Contains(ev.Text, "connection reset") || !strings.Contains(ev.Text, "attempt 1 of 4") {
		t.Fatalf("retry notice = %q", ev.Text)
	}
	var found bool
	for _, b := range srv.snapshotEvent().Blocks {
		if b.Kind == kindSystem && b.Title == "retrying" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the retry should be in the transcript: %+v", srv.snapshotEvent().Blocks)
	}
}

func TestSwitchingWhileRunningIsNotReportedAsAFailure(t *testing.T) {
	id := "aaaa000011112222"
	fake := &fakeAgent{blocking: true}
	srv := newTestServer(t, fake, func(o *Options) {
		o.SessionID = func() string { return id }
		o.OpenSession = func(want string) (string, error) {
			id = want
			return "(" + want + ")", nil
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"long task"}`)
	resp.Body.Close()
	// Wait until the turn is actually running, then switch out from under it.
	deadline := time.Now().Add(2 * time.Second)
	for !srv.isRunning() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	resp = post(t, ts.URL, srv.Token(), "/api/session/open", `{"id":"bbbb111122223333"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch status = %d", resp.StatusCode)
	}
	if err := srv.OpenSession("cccc222233334444"); err != nil {
		t.Fatalf("second switch: %v", err)
	}

	state := stream.waitFor(t, "state")
	for _, b := range state.Blocks {
		if b.Kind == kindError && strings.Contains(b.Body, "context canceled") {
			t.Fatalf("the switch's abort must not be reported as a failure: %+v", state.Blocks)
		}
	}
	if srv.isRunning() {
		t.Fatal("the aborted turn should have finished")
	}
}

// TestSwitchDuringARunDoesNotLeakTheOldTurn is the race the switcher has to
// survive: a turn is streaming when the console switches to another session, and
// the events of the abandoned run must not land in the conversation that was
// opened.
func TestSwitchDuringARunDoesNotLeakTheOldTurn(t *testing.T) {
	ag := &fakeAgent{blocking: true}
	id := "aaaa000011112222"
	srv := newTestServer(t, ag, func(o *Options) {
		o.SessionID = func() string { return id }
		o.OpenSession = func(want string) (string, error) {
			ag.setHistory(agent.NewUserMessage("stored in " + want))
			id = want
			return "(" + want + ")", nil
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	// Start a turn that blocks until it is aborted, and let it reach the point
	// where it is streaming.
	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"slow work"}`)
	resp.Body.Close()
	stream.waitFor(t, "user")
	if !srv.isRunning() {
		t.Fatal("the run should be in flight")
	}

	// Switch while it runs; the server must abort the turn and wait it out.
	openResp := post(t, ts.URL, srv.Token(), "/api/session/open", `{"id":"bbbb111122223333"}`)
	openResp.Body.Close()
	if openResp.StatusCode != http.StatusOK {
		t.Fatalf("open status = %d", openResp.StatusCode)
	}
	if !ag.wasAborted() {
		t.Fatal("switching must abort the running turn")
	}
	if srv.isRunning() {
		t.Fatal("the switch must wait for the aborted run to finish")
	}
	if got := srv.SessionID(); got != "bbbb111122223333" {
		t.Fatalf("session = %q", got)
	}

	// The transcript must be the opened session, not the abandoned turn.
	state := srv.snapshotEvent()
	for _, b := range state.Blocks {
		if b.Body == "slow work" {
			t.Fatalf("the abandoned turn leaked into the opened session: %+v", state.Blocks)
		}
	}
	if len(state.Blocks) == 0 || state.Blocks[0].Body != "stored in bbbb111122223333" {
		t.Fatalf("transcript not rebuilt for the opened session: %+v", state.Blocks)
	}
}

// TestSwitchDropsTheQueueOfTheAbandonedTurn checks the queued prompts of the old
// conversation do not run against the new one.
func TestSwitchDropsTheQueueOfTheAbandonedTurn(t *testing.T) {
	ag := &fakeAgent{blocking: true}
	id := "aaaa000011112222"
	srv := newTestServer(t, ag, func(o *Options) {
		o.SessionID = func() string { return id }
		o.OpenSession = func(want string) (string, error) {
			id = want
			return "", nil
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	resp := post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"first"}`)
	resp.Body.Close()
	stream.waitFor(t, "user")

	// Queue a second prompt, then switch away.
	resp = post(t, ts.URL, srv.Token(), "/api/prompt", `{"text":"queued for the old session"}`)
	resp.Body.Close()
	stream.waitFor(t, "system") // the "queued" notice

	openResp := post(t, ts.URL, srv.Token(), "/api/session/open", `{"id":"bbbb111122223333"}`)
	openResp.Body.Close()

	// Give the (now empty) queue a moment; the abandoned prompt must not run.
	time.Sleep(50 * time.Millisecond)
	ag.mu.Lock()
	prompts := append([]string(nil), ag.prompts...)
	ag.mu.Unlock()
	for _, p := range prompts {
		if p == "queued for the old session" {
			t.Fatalf("a queued prompt of the abandoned turn ran: %v", prompts)
		}
	}
	if srv.isRunning() {
		t.Fatal("no turn should be running after the switch")
	}
}

func TestSnapshotCarriesTheContextFigures(t *testing.T) {
	fa := &fakeAgent{ctxInfo: agent.ContextInfo{
		Used: 12000, Limit: 128000, Exact: true, Source: agent.LimitFromTable,
		AutoCompact: true, CompactAt: 80,
	}}
	srv := newTestServer(t, fa)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	snapshot := stream.waitFor(t, "state")
	if snapshot.CtxUsed != 12000 || snapshot.CtxLimit != 128000 || !snapshot.CtxExact {
		t.Fatalf("context figures missing from the snapshot: %+v", snapshot)
	}
	if snapshot.CtxSource != agent.LimitFromTable || snapshot.CtxCompactAt != 80 {
		t.Fatalf("provenance missing from the snapshot: %+v", snapshot)
	}
}

func TestCompactEndpointRunsAndBroadcasts(t *testing.T) {
	fa := &fakeAgent{}
	srv := newTestServer(t, fa)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	stream.waitFor(t, "state")

	resp := post(t, ts.URL, srv.Token(), "/api/compact", `{"instructions":"keep the parser work"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status = %d", resp.StatusCode)
	}

	fa.mu.Lock()
	got := append([]string(nil), fa.compacts...)
	fa.mu.Unlock()
	if len(got) != 1 || got[0] != "keep the parser work" {
		t.Fatalf("instructions were not passed: %v", got)
	}

	ev := stream.waitForMatch(t, func(e outEvent) bool { return e.Compaction != nil }, "a compaction event")
	if ev.Title != "context compacted" || ev.Compaction.Messages != 9 {
		t.Fatalf("the page was not told: %+v", ev)
	}
	if !strings.Contains(ev.Text, "9 messages summarized") {
		t.Fatalf("notification body: %q", ev.Text)
	}
}

func TestCompactEndpointQueuesWhileRunning(t *testing.T) {
	release := make(chan struct{})
	fa := &fakeAgent{blocking: true, release: release}
	srv := newTestServer(t, fa)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if _, err := srv.Submit("first"); err != nil {
		t.Fatal(err)
	}

	resp := post(t, ts.URL, srv.Token(), "/api/compact", `{}`)
	var res map[string]bool
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("compact status = %d", resp.StatusCode)
	}
	if !res["queued"] {
		t.Fatalf("compaction should be queued during a run: %v", res)
	}
	fa.mu.Lock()
	n := len(fa.compacts)
	fa.mu.Unlock()
	if n != 0 {
		t.Fatalf("it must not compact mid-turn: %d", n)
	}

	// The queued compaction runs once the turn it waited for finished.
	close(release)
	waitFor(t, func() bool {
		fa.mu.Lock()
		defer fa.mu.Unlock()
		return len(fa.compacts) == 1
	}, "the queued compaction to run")
}

func TestCompactEndpointRequiresToken(t *testing.T) {
	fa := &fakeAgent{}
	srv := newTestServer(t, fa)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := post(t, ts.URL, "wrong", "/api/compact", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
	if len(fa.compacts) != 0 {
		t.Fatal("an unauthenticated request must not compact")
	}
}

func TestCompactionEventBecomesATranscriptBlock(t *testing.T) {
	fa := &fakeAgent{}
	srv := newTestServer(t, fa)
	info := agent.CompactionInfo{Messages: 18, Before: 105000, After: 24000, Automatic: true}
	srv.applyEvent(agent.Event{Type: agent.EventCompaction, Compaction: &info})

	srv.mu.Lock()
	defer srv.mu.Unlock()
	found := false
	for _, b := range srv.blocks {
		if b.Title == "context compacted" {
			found = true
			if !strings.Contains(b.Body, "18 messages summarized") || !strings.Contains(b.Body, "automatic") {
				t.Fatalf("block body: %q", b.Body)
			}
		}
	}
	if !found {
		t.Fatal("the compaction should appear in the transcript")
	}
}

func TestSnapshotCarriesTheSummaryAndLastRequest(t *testing.T) {
	fa := &fakeAgent{
		summary: "we fixed the parser",
		ctxInfo: agent.ContextInfo{
			Used: 12000, Limit: 128000, Exact: true, AutoCompact: true, CompactAt: 80,
			LastInput: 11980, LastOutput: 20,
		},
	}
	srv := newTestServer(t, fa)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	ev := stream.waitFor(t, "state")
	if ev.CtxSummary != "we fixed the parser" {
		t.Fatalf("/context summary would have nothing to print: %+v", ev)
	}
	if ev.CtxLastInput != 11980 || ev.CtxLastOutput != 20 {
		t.Fatalf("the last request is missing: %+v", ev)
	}
}

func TestCompactingIsReportedToThePage(t *testing.T) {
	fa := &fakeAgent{ctxInfo: agent.ContextInfo{
		Used: 105000, Limit: 128000, Exact: true, Compacting: true,
	}}
	srv := newTestServer(t, fa)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	if ev := stream.waitFor(t, "state"); !ev.CtxCompacting {
		t.Fatalf("the page should be told compaction is running: %+v", ev)
	}
}

func TestSessionMenuMarksACompactedSession(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, func(o *Options) {
		o.SessionID = func() string { return "aaaa000011112222" }
		o.ListSessions = func() []session.Info {
			return []session.Info{
				{ID: "aaaa000011112222", Messages: 9, Compacted: true},
				{ID: "bbbb111122223333", Messages: 4},
			}
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := post(t, ts.URL, srv.Token(), "/api/sessions", `{}`)
	defer resp.Body.Close()
	var body struct {
		Sessions []sessionInfo `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 2 || !body.Sessions[0].Compacted || body.Sessions[1].Compacted {
		t.Fatalf("compacted flags = %+v", body.Sessions)
	}
}

func TestHistoryBlocksShowTheSummaryNotTheCompactedMessages(t *testing.T) {
	fa := &fakeAgent{summary: "we fixed the parser", summarized: 4}
	fa.setHistory(
		agent.NewUserMessage("the recent question"),
		agent.Message{Role: agent.RoleAssistant, Content: "the recent answer"},
	)
	blocks := historyBlocks(fa)
	if len(blocks) == 0 {
		t.Fatal("no blocks")
	}
	if blocks[0].Title != "summary of earlier conversation" ||
		!strings.Contains(blocks[0].Body, "we fixed the parser") ||
		!strings.Contains(blocks[0].Body, "4 earlier messages") {
		t.Fatalf("the first block should be the summary: %+v", blocks[0])
	}
	foundKept := false
	for _, b := range blocks {
		if b.Body == "the recent question" {
			foundKept = true
		}
	}
	if !foundKept {
		t.Fatalf("the kept messages should still be replayed: %+v", blocks)
	}
}

// TestSnapshotCarriesTheAutomaticCompactionFlag pins that the page can tell
// "no compaction is configured" from "the threshold is 0": the breakdown must
// not claim automatic compaction is on when it is off.
func TestSnapshotCarriesTheAutomaticCompactionFlag(t *testing.T) {
	fa := &fakeAgent{ctxInfo: agent.ContextInfo{
		Used: 12000, Limit: 128000, CompactAt: 80, AutoCompact: false,
	}}
	srv := newTestServer(t, fa)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	stream := connectSSE(t, ts.URL, srv.Token())
	defer stream.close()
	snapshot := stream.waitFor(t, "state")
	if snapshot.CtxAutoCompact {
		t.Fatalf("automatic compaction is off, the page should know: %+v", snapshot)
	}
	if snapshot.CtxCompactAt != 80 {
		t.Fatalf("the threshold is still reported: %+v", snapshot)
	}
}

// TestQueuedCompactionIsDroppedByASessionSwitch pins §4.7: a compaction queued
// during a run belongs to the conversation that submitted it, so switching
// sessions drops it instead of compacting the one being opened.
func TestQueuedCompactionIsDroppedByASessionSwitch(t *testing.T) {
	release := make(chan struct{})
	fa := &fakeAgent{blocking: true, release: release}
	var opened string
	srv := newTestServer(t, fa, func(o *Options) {
		o.SessionID = func() string { return "current" }
		o.OpenSession = func(id string) (string, error) {
			opened = id
			return "session " + id, nil
		}
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if _, err := srv.Submit("first"); err != nil {
		t.Fatal(err)
	}
	resp := post(t, ts.URL, srv.Token(), "/api/compact", `{}`)
	var res map[string]bool
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if !res["queued"] {
		t.Fatalf("the compaction should have been queued: %v", res)
	}

	if err := srv.OpenSession("other"); err != nil {
		t.Fatal(err)
	}
	close(release)
	// Give the finished run every chance to run a compaction it should not.
	waitFor(t, func() bool { return !srv.isRunning() }, "the run to finish")
	time.Sleep(50 * time.Millisecond)

	if opened != "other" {
		t.Fatalf("session not opened: %q", opened)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.compacts) != 0 {
		t.Fatalf("a queued compaction must not run against the next session: %v", fa.compacts)
	}
}

// TestCompactionBlockReportsTheKeptTurns pins the notification's figures in the
// console as well as the terminal.
func TestCompactionBlockReportsTheKeptTurns(t *testing.T) {
	body := compactionBody(agent.CompactionInfo{
		Messages: 9, Before: 105000, After: 24000, KeptTurns: 2, Automatic: true,
	})
	if !strings.Contains(body, "the 2 most recent turns kept") {
		t.Fatalf("the block should report the turns it kept: %q", body)
	}
}

// posixFileMode reports a file's permission bits, and whether the host's
// filesystem actually carries them. Windows does not, so a test must not read
// the number there.
func posixFileMode(path string) (os.FileMode, bool) {
	if runtime.GOOS == "windows" {
		return 0, false
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return st.Mode().Perm(), true
}
