package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rushera/hal/internal/agent"
	"github.com/Rushera/hal/internal/config"
	"github.com/Rushera/hal/internal/provider"
	"github.com/Rushera/hal/internal/session"
	"github.com/Rushera/hal/internal/tools"
)

// newSessionFor creates a session in the configured store for the given cwd.
func newSessionFor(t *testing.T, cwd, name string) (*session.Session, error) {
	t.Helper()
	return session.New(session.RootWith(""), cwd, name, "mock")
}

// mockServer scripted like a provider: every response is a list of SSE chunks.
type mockServer struct {
	t        *testing.T
	script   [][]string
	requests []map[string]any
	headers  []http.Header
	server   *httptest.Server
	call     int
	// window, when positive, is the context window the /models endpoint
	// publishes; zero answers "nothing published".
	window int
}

func newMockServer(t *testing.T, script [][]string) *mockServer {
	m := &mockServer{t: t, script: script}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockServer) handle(w http.ResponseWriter, r *http.Request) {
	// The provider looks up the model's context window once, unless the config
	// supplied one. Most scripts do not exercise that, so the default answer is
	// "nothing published".
	if strings.HasSuffix(r.URL.Path, "/models") {
		if m.window <= 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":[{"id":"mock-model","context_length":%d}]}`, m.window)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		m.t.Errorf("mock received invalid JSON: %v", err)
	}
	m.requests = append(m.requests, req)
	m.headers = append(m.headers, r.Header.Clone())

	if m.call >= len(m.script) {
		m.t.Errorf("mock got more requests (%d) than scripted", m.call+1)
		http.Error(w, "no scripted response", http.StatusInternalServerError)
		return
	}
	chunks := m.script[m.call]
	m.call++

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
		if flusher != nil {
			flusher.Flush()
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func textChunks(texts ...string) []string {
	var out []string
	for _, s := range texts {
		out = append(out, fmt.Sprintf(`{"choices":[{"delta":{"content":%s},"finish_reason":null}]}`, mustJSON(s)))
	}
	out = append(out, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	return out
}

// toolChunks streams a tool call split across chunks (like real providers do).
func toolChunks(id, name, argsFragment1, argsFragment2 string) []string {
	return []string{
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%s,"function":{"name":%s,"arguments":%s}}]},"finish_reason":null}]}`,
			mustJSON(id), mustJSON(name), mustJSON(argsFragment1)),
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]},"finish_reason":null}]}`, mustJSON(argsFragment2)),
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":9,"total_tokens":29}}`,
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestAgentLoopExecutesToolAndFinishes is the core end-to-end test: a scripted
// provider asks for the write tool, the real tool runs against a real temp
// directory, and the follow-up request must contain the tool result.
func TestAgentLoopExecutesToolAndFinishes(t *testing.T) {
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		toolChunks("call_1", "write", `{"path":"hello.txt","content":"hi from the tool\n"}`, ``),
		textChunks("Done: ", "wrote hello.txt"),
	})

	prov := provider.NewFromEnv("test-key", mock.server.URL, "mock-model")
	if prov.Name() != "openai-compatible" {
		t.Fatalf("unexpected provider name %q", prov.Name())
	}
	toolSet, err := tools.Select(workdir, "")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(prov, toolSet, workdir, "")

	var events []agent.Event
	err = ag.RunPrompt(context.Background(), "create hello.txt", func(ev agent.Event) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("agent loop failed: %v", err)
	}

	// the tool really ran against the filesystem
	data, err := os.ReadFile(filepath.Join(workdir, "hello.txt"))
	if err != nil {
		t.Fatalf("tool did not write the file: %v", err)
	}
	if string(data) != "hi from the tool\n" {
		t.Fatalf("wrong file content: %q", data)
	}

	// the provider saw two requests: the tool result must be in the second
	if mock.call != 2 {
		t.Fatalf("expected 2 provider requests, got %d", mock.call)
	}
	second := mock.requests[1]
	msgs, _ := second["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("second request had no messages")
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" {
		t.Fatalf("last message should be the tool result, got %v", last["role"])
	}
	if last["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id not propagated: %v", last["tool_call_id"])
	}
	if !strings.Contains(fmt.Sprint(last["content"]), "wrote 17 bytes to hello.txt") {
		t.Fatalf("tool result content missing: %v", last["content"])
	}

	// tools were advertised on the first request
	first := mock.requests[0]
	declared, _ := first["tools"].([]any)
	if len(declared) != 7 {
		t.Fatalf("expected 7 tool definitions, got %d", len(declared))
	}
	if first["stream"] != true {
		t.Fatal("streaming was not requested")
	}

	// event stream shape: deltas, tool start/result, turn end, done, usage
	var sawDelta, sawToolStart, sawToolResult, sawTurnEnd, sawDone, sawUsage bool
	var streamed strings.Builder
	for _, ev := range events {
		switch ev.Type {
		case agent.EventTextDelta:
			sawDelta = true
			streamed.WriteString(ev.Text)
		case agent.EventToolStart:
			sawToolStart = true
			if ev.ToolCall == nil || ev.ToolCall.Name != "write" {
				t.Fatalf("bad tool start event: %+v", ev.ToolCall)
			}
		case agent.EventToolResult:
			sawToolResult = true
			if ev.Result == nil || ev.Result.IsError {
				t.Fatalf("tool result should be successful: %+v", ev.Result)
			}
		case agent.EventTurnEnd:
			sawTurnEnd = true
		case agent.EventUsage:
			sawUsage = true
		case agent.EventDone:
			sawDone = true
		}
	}
	if !sawDelta || !sawToolStart || !sawToolResult || !sawTurnEnd || !sawUsage || !sawDone {
		t.Fatalf("missing events: delta=%v toolStart=%v toolResult=%v turnEnd=%v usage=%v done=%v",
			sawDelta, sawToolStart, sawToolResult, sawTurnEnd, sawUsage, sawDone)
	}
	if streamed.String() != "Done: wrote hello.txt" {
		t.Fatalf("streamed text mismatch: %q", streamed.String())
	}

	// History is the conversation only: the system prompt is added when the
	// request is assembled, not stored in history.
	hist := ag.Messages()
	if len(hist) != 4 {
		t.Fatalf("expected 4 history messages, got %d: %+v", len(hist), roles(hist))
	}
	if hist[1].Role != agent.RoleAssistant || len(hist[1].ToolCalls) != 1 || hist[1].ToolCalls[0].Name != "write" {
		t.Fatalf("assistant tool call not recorded: %+v", hist[1])
	}
	if hist[2].Role != agent.RoleTool || hist[2].Content == "" {
		t.Fatalf("tool result not recorded: %+v", hist[2])
	}
	if hist[3].Role != agent.RoleAssistant || hist[3].Content != "Done: wrote hello.txt" {
		t.Fatalf("final assistant message not recorded: %+v", hist[3])
	}
	// the system prompt is still prepended to requests
	if msgs, _ := mock.requests[0]["messages"].([]any); len(msgs) == 0 {
		t.Fatal("no messages sent")
	} else if first, _ := msgs[0].(map[string]any); first["role"] != "system" {
		t.Fatalf("first request message should be the system prompt, got %v", first["role"])
	}
	if !strings.Contains(ag.SystemPrompt, workdir) {
		t.Fatalf("system prompt should mention the working directory")
	}
}

func roles(msgs []agent.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

// TestAgentLoopChainsMultipleTools covers read -> edit in one run.
func TestAgentLoopChainsMultipleTools(t *testing.T) {
	workdir := t.TempDir()
	target := filepath.Join(workdir, "config.go")
	if err := os.WriteFile(target, []byte("timeout := 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := newMockServer(t, [][]string{
		toolChunks("c1", "read", `{"path":"config.go"}`, ``),
		toolChunks("c2", "edit", `{"path":"config.go",`, `"oldText":"timeout := 30","newText":"timeout := 60"}`),
		textChunks("Bumped the timeout to 60s."),
	})

	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")

	if err := ag.RunPrompt(context.Background(), "bump the timeout", func(agent.Event) {}); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "timeout := 60\n" {
		t.Fatalf("edit not applied: %q", data)
	}
	if mock.call != 3 {
		t.Fatalf("expected 3 provider requests, got %d", mock.call)
	}
}

// TestToolFailureIsFedBackToTheModel checks errors become tool results, not
// aborts.
func TestToolFailureIsFedBackToTheModel(t *testing.T) {
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		toolChunks("c1", "read", `{"path":"missing.txt"}`, ``),
		textChunks("That file does not exist."),
	})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")

	var sawError bool
	err := ag.RunPrompt(context.Background(), "read missing.txt", func(ev agent.Event) {
		if ev.Type == agent.EventToolResult && ev.Result != nil && ev.Result.IsError {
			sawError = true
		}
	})
	if err != nil {
		t.Fatalf("a failing tool must not abort the run: %v", err)
	}
	if !sawError {
		t.Fatal("expected an error tool result")
	}
	last := ag.Messages()[len(ag.Messages())-1]
	if !strings.Contains(last.Content, "does not exist") && last.Role != agent.RoleAssistant {
		t.Fatalf("unexpected last message: %+v", last)
	}
}

// TestToolAllowlistLimitsAdvertisedTools covers `--tools read,grep`.
func TestToolAllowlistLimitsAdvertisedTools(t *testing.T) {
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{textChunks("nothing to do")})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, err := tools.Select(workdir, "read,grep")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(prov, toolSet, workdir, "")
	if err := ag.RunPrompt(context.Background(), "hi", func(agent.Event) {}); err != nil {
		t.Fatal(err)
	}
	declared, _ := mock.requests[0]["tools"].([]any)
	if len(declared) != 2 {
		t.Fatalf("expected 2 tools advertised, got %d", len(declared))
	}
}

// TestProviderHTTPErrorIsReported checks a non-2xx response surfaces cleanly.
func TestProviderHTTPErrorIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	prov := provider.NewFromEnv("k", srv.URL, "mock")
	ag := agent.New(prov, nil, t.TempDir(), "")
	err := ag.RunPrompt(context.Background(), "hi", func(agent.Event) {})
	if err == nil {
		t.Fatal("expected an error for HTTP 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error should mention the status: %v", err)
	}
}

// TestParseArgs covers CLI flag handling.
func TestParseArgs(t *testing.T) {
	o, set, err := parseArgs([]string{"-p", "--model", "gpt-4o", "--tools", "read,grep", "@notes.md", "summarize", "this"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.printMode || o.model != "gpt-4o" || o.toolList != "read,grep" {
		t.Fatalf("flags not parsed: %+v", o)
	}
	if !set["p"] || !set["model"] || !set["tools"] {
		t.Fatalf("explicitly set flags not tracked: %v", set)
	}
	if set["no-session"] {
		t.Fatalf("flags that were not typed must not be recorded as set: %v", set)
	}
	if len(o.files) != 1 || o.files[0] != "notes.md" {
		t.Fatalf("file arg not parsed: %+v", o.files)
	}
	if strings.Join(o.prompts, " ") != "summarize this" {
		t.Fatalf("prompt not assembled: %+v", o.prompts)
	}

	if _, _, err := parseArgs([]string{"--mode", "yaml"}); err == nil {
		t.Fatal("expected an error for an unknown mode")
	}

	// an unsupported provider is rejected after resolution, not during parsing
	o, _, err = parseArgs([]string{"--provider", "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateResolved(o); err == nil {
		t.Fatal("expected validateResolved to reject a non-bundled provider")
	}
}

func TestBuildPromptMergesFilesAndStdin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("- [ ] task one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := buildPrompt([]string{"extract the todos"}, []string{"notes.md"}, dir, "PIPED CONTENT")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PIPED CONTENT", `<file path="notes.md">`, "task one", "extract the todos"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
	if _, err := buildPrompt(nil, []string{"nope.md"}, dir, ""); err == nil {
		t.Fatal("expected an error for a missing @file")
	}
}

// TestRunPrintStreamsToStdout exercises the print-mode path end to end.
func TestRunPrintStreamsToStdout(t *testing.T) {
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		toolChunks("c1", "bash", `{"command":"echo tool-ran > out.txt"}`, ``),
		textChunks("Ran ", "the command."),
	})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")

	var stdout, stderr bytes.Buffer
	if err := runPrint(context.Background(), ag, "run echo", &stdout, &stderr); err != nil {
		t.Fatalf("print mode failed: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "Ran the command.") {
		t.Fatalf("stdout should stream the assistant text, got %q", got)
	}
	if !strings.Contains(stderr.String(), "bash(") {
		t.Fatalf("stderr should report tool activity, got %q", stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(workdir, "out.txt"))
	if err != nil || !strings.Contains(string(data), "tool-ran") {
		t.Fatalf("bash tool did not execute: %v %q", err, data)
	}
}

// TestRunJSONEmitsEventStream covers `--mode json`.
func TestRunJSONEmitsEventStream(t *testing.T) {
	mock := newMockServer(t, [][]string{textChunks("json mode works")})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	ag := agent.New(prov, nil, t.TempDir(), "")

	out := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := runJSON(context.Background(), ag, "hi")
	w.Close()
	os.Stdout = out
	if runErr != nil {
		t.Fatal(runErr)
	}
	data, _ := io.ReadAll(r)

	var types []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev agent.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid JSON line %q: %v", line, err)
		}
		types = append(types, ev.Type)
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, agent.EventTextDelta) || !strings.Contains(joined, agent.EventDone) {
		t.Fatalf("event stream incomplete: %s", joined)
	}
}

func TestContextFilesIncludeGlobalConfigDir(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	workdir := t.TempDir()

	globalDir := filepath.Join(cfgHome, "hal")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "AGENTS.md"), []byte("global rules"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := contextFiles(workdir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if strings.HasSuffix(f.path, filepath.Join("hal", "AGENTS.md")) {
			found = true
			if !strings.Contains(f.label, "XDG_CONFIG_HOME") {
				t.Fatalf("global context file not labelled as the config dir: %q", f.label)
			}
		}
	}
	if !found {
		t.Fatalf("AGENTS.md in $XDG_CONFIG_HOME/hal was not loaded: %+v", files)
	}

	// the content reaches the system prompt
	opts := options{}
	prompt, err := buildSystemPrompt(workdir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "global rules") {
		t.Fatalf("global instructions missing from the system prompt:\n%s", prompt)
	}

	// --no-context-files suppresses them
	opts.noContext = true
	prompt, err = buildSystemPrompt(workdir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "global rules") {
		t.Fatal("--no-context-files should suppress context files")
	}
}

func TestContextFilesDiscovery(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "project")
	if err := os.MkdirAll(filepath.Join(child, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "AGENTS.md"), []byte("parent rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "AGENTS.md"), []byte("child rules"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := contextFiles(filepath.Join(child, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	var foundParent, foundChild bool
	for _, f := range files {
		if strings.HasSuffix(f.path, filepath.Join(parent, "AGENTS.md")) {
			foundParent = true
		}
		if strings.HasSuffix(f.path, filepath.Join(child, "AGENTS.md")) {
			foundChild = true
		}
	}
	if !foundParent || !foundChild {
		t.Fatalf("expected both context files, got %+v", files)
	}
	if len(files) > 1 && !strings.Contains(files[len(files)-1].path, "sub") {
		// the innermost directory must be searched last so it wins
		for i := 0; i < len(files)-1; i++ {
			if strings.Contains(files[i].path, "child") && strings.Contains(files[i+1].path, "parent") {
				t.Fatalf("context files not ordered outer-to-inner: %+v", files)
			}
		}
	}
}

func TestSessionPersistenceThroughAgent(t *testing.T) {
	root := t.TempDir()
	workdir := t.TempDir()
	t.Setenv("HAL_SESSION_DIR", root)

	mock := newMockServer(t, [][]string{textChunks("noted")})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")

	sess, err := newSessionFor(t, workdir, "cli session")
	if err != nil {
		t.Fatal(err)
	}
	ag.OnMessage = func(m agent.Message) { _ = sess.AppendMessage(m) }
	if err := ag.RunPrompt(context.Background(), "remember this", func(agent.Event) {}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 { // session header, user, assistant
		t.Fatalf("expected 3 session lines, got %d:\n%s", len(lines), data)
	}
	if !strings.Contains(lines[0], `"type":"session"`) || !strings.Contains(lines[1], `"role":"user"`) {
		t.Fatalf("unexpected session file:\n%s", data)
	}
}

// resetServer accepts connections and immediately drops them, which is what the
// "read: connection reset by peer" failure looks like from the client side.
type resetServer struct {
	*httptest.Server
	hits int32
}

func newResetServer(t *testing.T, resets int) *resetServer {
	t.Helper()
	rs := &resetServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		if int(atomic.AddInt32(&rs.hits, 1)) <= resets {
			// Close the connection under the client without a response.
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range textChunks("recovered at last") {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(rs.Close)
	return rs
}

// TestAgentSurvivesResetConnections is the end-to-end version of the reported
// bug: a gateway that resets the first connections must not fail the turn.
func TestAgentSurvivesResetConnections(t *testing.T) {
	workdir := t.TempDir()
	rs := newResetServer(t, 2)
	prov := provider.NewFromEnv("k", rs.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")
	ag.RetryBaseDelay = time.Millisecond // keep the test fast
	var sleeps []time.Duration
	ag.Sleep = func(ctx context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }

	var text strings.Builder
	var retries int
	err := ag.RunPrompt(context.Background(), "hello", func(ev agent.Event) {
		switch ev.Type {
		case agent.EventTextDelta:
			text.WriteString(ev.Text)
		case agent.EventRetry:
			retries++
		case agent.EventError:
			t.Fatalf("the turn must not fail: %s", ev.Error)
		}
	})
	if err != nil {
		t.Fatalf("the turn should have recovered: %v", err)
	}
	if text.String() != "recovered at last" {
		t.Fatalf("answer = %q", text.String())
	}
	if retries != 2 {
		t.Fatalf("retry events = %d, want 2", retries)
	}
	if len(sleeps) != 2 {
		t.Fatalf("backoff sleeps = %v", sleeps)
	}
	if got := atomic.LoadInt32(&rs.hits); got != 3 {
		t.Fatalf("server hits = %d, want 3", got)
	}
	// The aborted attempts must not have polluted the conversation.
	msgs := ag.Messages()
	if len(msgs) != 2 || msgs[0].Role != agent.RoleUser || msgs[1].Content != "recovered at last" {
		t.Fatalf("history = %+v", msgs)
	}
}

// TestAgentReportsPersistentResetConnections checks the failure surfaces with
// the transport detail once the retries are exhausted.
func TestAgentReportsPersistentResetConnections(t *testing.T) {
	workdir := t.TempDir()
	rs := newResetServer(t, 99)
	prov := provider.NewFromEnv("k", rs.URL, "mock")
	toolSet, _ := tools.Select(workdir, "")
	ag := agent.New(prov, toolSet, workdir, "")
	ag.MaxRetries = 2
	ag.RetryBaseDelay = time.Millisecond
	ag.Sleep = func(ctx context.Context, d time.Duration) error { return nil }

	err := ag.RunPrompt(context.Background(), "hello", func(agent.Event) {})
	if err == nil {
		t.Fatal("expected the run to fail when every attempt resets")
	}
	// The user should see why, not just "EOF".
	if !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("the transport failure should be reported: %v", err)
	}
	if got := atomic.LoadInt32(&rs.hits); got != 3 { // 1 try + 2 retries
		t.Fatalf("server hits = %d, want 3", got)
	}
}

// usageChunks answers with usage high enough to fill a small window, so the
// next request has to be preceded by a compaction.
func toolChunksUsage(id, name, args, rest string, promptTokens int) []string {
	return []string{
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%s,"function":{"name":%s,"arguments":%s}}]},"finish_reason":null}]}`,
			mustJSON(id), mustJSON(name), mustJSON(args)),
		fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":%d,"completion_tokens":10,"total_tokens":%d}}`,
			promptTokens, promptTokens+10),
	}
}

func usageChunks(text string, promptTokens int) []string {
	return []string{
		fmt.Sprintf(`{"choices":[{"delta":{"content":%s},"finish_reason":null}]}`, mustJSON(text)),
		fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":10,"total_tokens":%d}}`,
			promptTokens, promptTokens+10),
	}
}

// TestCompactionKeepsTheRunGoing pins the end-to-end behaviour: once the
// provider's reported usage crosses the threshold, the next request carries a
// summary instead of the messages it replaced, and the run still finishes.
func TestCompactionKeepsTheRunGoing(t *testing.T) {
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		// Turn 1 reports a prompt far above the threshold below.
		toolChunksUsage("call_1", "write", `{"path":"a.txt","content":"one\n"}`, ``, 7000),
		// The summarization request compaction makes before turn 2.
		textChunks("the summary of the earlier work"),
		// Turn 2 answers.
		usageChunks("done after compaction", 50),
	})

	prov := provider.NewFromEnv("k", mock.server.URL, "mock-model")
	toolSet, err := tools.Select(workdir, "")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(prov, toolSet, workdir, "")
	// Four turns of history with distinct text: the last three are kept, the
	// first is summarized.
	for i := 0; i < 4; i++ {
		ag.History = append(ag.History,
			agent.Message{Role: agent.RoleUser, Content: fmt.Sprintf("TURN%d user %s", i+1, strings.Repeat("work ", 8))},
			agent.Message{Role: agent.RoleAssistant, Content: fmt.Sprintf("TURN%d answer %s", i+1, strings.Repeat("done ", 8))},
		)
	}
	ag.ContextWindowOverride = 8000
	ag.CompactAt = 30 // 2400 tokens: the start is below it, turn 1 is above it

	var sawCompaction bool
	err = ag.RunPrompt(context.Background(), "keep going", func(ev agent.Event) {
		if ev.Type == agent.EventCompaction && ev.Compaction != nil && ev.Compaction.Error == "" {
			sawCompaction = true
		}
	})
	if err != nil {
		t.Fatalf("the run should survive compaction: %v", err)
	}
	if !sawCompaction {
		t.Fatalf("the context exceeded the threshold, so a compaction should have run")
	}

	// The follow-up request must carry the summary and not the summarized text.
	if len(mock.requests) < 3 {
		t.Fatalf("expected turn 1, the summary, and turn 2; got %d requests", len(mock.requests))
	}
	body := fmt.Sprint(mock.requests[len(mock.requests)-1]["messages"])
	if !strings.Contains(body, "summary of the conversation so far") {
		t.Fatalf("the request should carry the summary:\n%s", body)
	}
	// The kept tail starts at the second turn, so the first turn's text must be
	// gone from the request even though it is still in the history.
	if strings.Contains(body, "TURN1") {
		t.Fatalf("a summarized message was sent again:\n%s", body)
	}
	if !strings.Contains(body, "TURN4") {
		t.Fatalf("the kept turns should still be sent:\n%s", body)
	}
}

// TestContextWindowFlagAndConfig covers the three settings end to end.
func TestContextWindowSettingsResolve(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte(
		"context_window = \"64k\"\nauto_compact = false\ncompact_at = 55\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))

	o, set, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	paths := config.Resolve()
	loaded, err := config.Load(cfg, paths)
	if err != nil {
		t.Fatal(err)
	}
	o, _ = resolveOptions(o, set, loaded)
	if o.contextWin != 64000 {
		t.Fatalf("a size suffix should be parsed: %d", o.contextWin)
	}
	if o.autoCompact {
		t.Fatal("auto_compact = false should be honored")
	}
	if o.compactAt != 55 {
		t.Fatalf("compact_at = %d", o.compactAt)
	}

	// A flag beats the file, including for a value of zero.
	o2, set2, err := parseArgs([]string{"--context-window", "200000", "--compact-at", "90"})
	if err != nil {
		t.Fatal(err)
	}
	o2, _ = resolveOptions(o2, set2, loaded)
	if o2.contextWin != 200000 || o2.compactAt != 90 {
		t.Fatalf("flags should win: %d %d", o2.contextWin, o2.compactAt)
	}
}

func TestInvalidContextSettingsAreRejected(t *testing.T) {
	// The threshold is validated after resolution, so a bad value coming from
	// the config file is rejected too, naming the setting.
	for _, args := range [][]string{
		{"--compact-at", "0"},
		{"--compact-at", "101"},
	} {
		o, set, err := parseArgs(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		o, _ = resolveOptions(o, set, config.Loaded{})
		if err := validateResolved(o); err == nil {
			t.Fatalf("%v should be rejected", args)
		}
	}
	// A negative window is rejected where it is parsed, since no setting can
	// make it valid.
	if _, _, err := parseArgs([]string{"--context-window", "-1"}); err == nil {
		t.Fatal("a negative window should be rejected")
	}
	o, set, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	o, _ = resolveOptions(o, set, config.Loaded{File: config.File{CompactAt: 0}})
	if err := validateResolved(o); err != nil {
		t.Fatalf("a zero compact_at should fall back to the default: %v", err)
	}
}

func TestAutoCompactFlagDefaultsOnAndTheNegativeFormWorks(t *testing.T) {
	o, set, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	o, _ = resolveOptions(o, set, config.Loaded{})
	if !o.autoCompact {
		t.Fatal("automatic compaction is on by default")
	}

	o, set, err = parseArgs([]string{"--no-auto-compact"})
	if err != nil {
		t.Fatal(err)
	}
	o, _ = resolveOptions(o, set, config.Loaded{})
	if o.autoCompact {
		t.Fatal("--no-auto-compact should turn it off")
	}

	// The config key is honoured when no flag says otherwise, and a flag wins.
	off := false
	o, set, err = parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	o, _ = resolveOptions(o, set, config.Loaded{File: config.File{AutoCompact: &off}})
	if o.autoCompact {
		t.Fatal("auto_compact = false should be honored")
	}
	o, set, err = parseArgs([]string{"--auto-compact"})
	if err != nil {
		t.Fatal(err)
	}
	o, _ = resolveOptions(o, set, config.Loaded{File: config.File{AutoCompact: &off}})
	if !o.autoCompact {
		t.Fatal("an explicit --auto-compact should beat the config file")
	}
}

// TestPrintModeReportsCompactionOnStderr pins where the notice goes: stdout is
// the answer alone, and the notice goes where tool activity already goes.
func TestPrintModeReportsCompactionOnStderr(t *testing.T) {
	mock := newMockServer(t, [][]string{
		toolChunksUsage("call_1", "write", `{"path":"a.txt","content":"one\n"}`, ``, 7000),
		textChunks("the summary"),
		usageChunks("done", 50),
	})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock-model")
	toolSet, err := tools.Select(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(prov, toolSet, t.TempDir(), "")
	ag.History = []agent.Message{
		agent.NewUserMessage("earlier work"),
		{Role: agent.RoleAssistant, Content: "earlier answer"},
		agent.NewUserMessage("more work"),
		{Role: agent.RoleAssistant, Content: "more answer"},
		agent.NewUserMessage("last work"),
		{Role: agent.RoleAssistant, Content: "last answer"},
		agent.NewUserMessage("final prompt"),
		{Role: agent.RoleAssistant, Content: "final answer"},
	}
	ag.ContextWindowOverride = 8000
	ag.CompactAt = 30

	var out, errOut strings.Builder
	if err := runPrint(context.Background(), ag, "keep going", &out, &errOut); err != nil {
		t.Fatalf("the run should survive compaction: %v", err)
	}
	if !strings.Contains(errOut.String(), "context compacted") {
		t.Fatalf("the notice should go to stderr: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "messages summarized") {
		t.Fatalf("the notice should say what happened: %q", errOut.String())
	}
	if strings.Contains(out.String(), "context compacted") {
		t.Fatalf("stdout should carry the answer alone: %q", out.String())
	}
}

// TestContextWindowFlagAcceptsSizes pins the form the help text promises: the
// flag and the config key take the same suffix.
func TestContextWindowFlagAcceptsSizes(t *testing.T) {
	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"--context-window", "128k"}, 128000},
		{[]string{"--context-window", "1m"}, 1000000},
		{[]string{"--context-window", "32000"}, 32000},
	} {
		o, _, err := parseArgs(c.args)
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if o.contextWin != c.want {
			t.Fatalf("%v = %d, want %d", c.args, o.contextWin, c.want)
		}
	}
	if _, _, err := parseArgs([]string{"--context-window", "12x"}); err == nil {
		t.Fatal("a bad suffix should be rejected at parse time")
	}
}

// TestEndpointWindowIsUsedWhenNothingOverridesIt pins §3.3 end to end: with no
// --context-window, the window the endpoint publishes is what the run compacts
// against, even though the console resolved a table figure before the run.
func TestEndpointWindowIsUsedWhenNothingOverridesIt(t *testing.T) {
	mock := newMockServer(t, [][]string{
		usageChunks("done", 50000),
	})
	mock.window = 32000
	prov := provider.NewFromEnv("k", mock.server.URL, "mock-model")
	ag := agent.New(prov, nil, t.TempDir(), "")

	// This is what a terminal or web console does before the first run.
	if got, _ := ag.ContextWindow(); got == 32000 {
		t.Fatalf("the footer cannot ask the endpoint, got %d", got)
	}

	var info agent.ContextInfo
	if err := ag.RunPrompt(context.Background(), "hi", func(ev agent.Event) {
		if ev.Type == agent.EventContext && ev.Context != nil {
			info = *ev.Context
		}
	}); err != nil {
		t.Fatal(err)
	}
	if info.Limit != 32000 || info.Source != agent.LimitFromProvider {
		t.Fatalf("the run should use the endpoint's window: %d %s", info.Limit, info.Source)
	}
}

// TestCompactionSurvivesAResume pins the stored format end to end: a compaction
// written by one session is restored by the next as the summary plus the tail,
// including which tool results were shortened.
func TestCompactionSurvivesAResume(t *testing.T) {
	workdir := t.TempDir()
	root := t.TempDir()
	t.Setenv("HAL_SESSION_DIR", root)

	mock := newMockServer(t, [][]string{textChunks("the summary")})
	mock.window = 100000
	prov := provider.NewFromEnv("k", mock.server.URL, "mock-model")
	ag := agent.New(prov, nil, workdir, "")
	ag.ContextWindowOverride = 100000

	sess, err := newSessionFor(t, workdir, "resumed")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		msg := agent.NewUserMessage(fmt.Sprintf("turn %d %s", i, strings.Repeat("work ", 8)))
		if err := sess.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
		ag.History = append(ag.History, msg)
		answer := agent.Message{Role: agent.RoleAssistant, Content: fmt.Sprintf("answer %d", i)}
		if err := sess.AppendMessage(answer); err != nil {
			t.Fatal(err)
		}
		ag.History = append(ag.History, answer)
	}
	ag.SetHistory(ag.History)
	ag.OnCompaction = func(c agent.Compacted) {
		if err := sess.AppendCompaction(session.Compaction{
			Summary:      c.Summary,
			Messages:     c.Messages,
			Shortened:    c.Shortened,
			ShortenedIDs: sess.MessageIDs(c.ShortenedIndexes),
		}, sess.MessageID(c.Through)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ag.Compact(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	path := sess.Path
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := session.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 12 {
		t.Fatalf("the log must stay complete, got %d messages", len(loaded.Messages))
	}
	restored := agent.New(prov, nil, workdir, "")
	restored.SetHistory(loaded.Messages)
	restoreCompaction(restored, loaded)
	summary, msgs, summarized := restored.ActiveContext()
	if summary != "the summary" {
		t.Fatalf("the summary did not survive the resume: %q", summary)
	}
	if len(msgs) != 6 || summarized != 6 {
		t.Fatalf("active context after resume: %d messages, %d summarized", len(msgs), summarized)
	}
	if used, _ := restored.UsedTokens(); used == 0 {
		t.Fatal("the restored conversation should have a figure")
	}
}

// TestResumeKeepsShortenedToolResults pins the one thing the log alone cannot
// reconstruct: a tool result the compaction replaced with a marker must still be
// replaced after a resume, or the context would grow back.
func TestResumeKeepsShortenedToolResults(t *testing.T) {
	workdir := t.TempDir()
	root := t.TempDir()
	t.Setenv("HAL_SESSION_DIR", root)

	mock := newMockServer(t, [][]string{textChunks("the summary"), textChunks("done")})
	prov := provider.NewFromEnv("k", mock.server.URL, "mock-model")
	ag := agent.New(prov, nil, workdir, "")
	// The summary target is half the window, so the kept tail (the last two
	// turns, with large tool results) must exceed it while the summarized part
	// still fits in one request.
	ag.ContextWindowOverride = 4000
	ag.KeepTurns = 2

	sess, err := newSessionFor(t, workdir, "shortened")
	if err != nil {
		t.Fatal(err)
	}
	var history []agent.Message
	for i := 0; i < 4; i++ {
		output := "small"
		if i >= 2 {
			output = strings.Repeat("t", 4000)
		}
		history = append(history,
			agent.NewUserMessage(fmt.Sprintf("turn %d", i)),
			agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "c", Name: "read", Arguments: "{}"}}},
			agent.Message{Role: agent.RoleTool, Content: output, ToolCallID: "c", ToolName: "read"},
			agent.Message{Role: agent.RoleAssistant, Content: "ok"},
		)
	}
	for _, m := range history {
		if err := sess.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	ag.SetHistory(history)
	ag.OnCompaction = func(c agent.Compacted) {
		if err := sess.AppendCompaction(session.Compaction{
			Summary:      c.Summary,
			Messages:     c.Messages,
			Shortened:    c.Shortened,
			ShortenedIDs: sess.MessageIDs(c.ShortenedIndexes),
		}, sess.MessageID(c.Through)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := ag.Compact(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Shortened == 0 {
		t.Fatal("the tail is larger than the target, so a tool result should have been shortened")
	}
	path := sess.Path
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := session.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	restored := agent.New(prov, nil, workdir, "")
	restored.SetHistory(loaded.Messages)
	restoreCompaction(restored, loaded)
	if err := restored.RunPrompt(context.Background(), "continue", func(agent.Event) {}); err != nil {
		t.Fatal(err)
	}
	// The request the resumed run sent is what the model sees; the shortened
	// tool result must still be a marker there, or the context would grow back
	// to the size the compaction was freed from.
	sent := fmt.Sprint(mock.requests[len(mock.requests)-1]["messages"])
	if !strings.Contains(sent, "[output omitted") {
		t.Fatalf("a resumed context should still send the shortened marker:\n%s", sent)
	}
}
