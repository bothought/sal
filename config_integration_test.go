package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bothought/sal/internal/agent"
	"github.com/bothought/sal/internal/config"
	"github.com/bothought/sal/internal/session"
)

// isolateXDG points every XDG variable at a temp directory so tests never read
// or write the developer's real configuration.
func isolateXDG(t *testing.T) (home, cfgHome, dataHome string) {
	t.Helper()
	home = t.TempDir()
	cfgHome = filepath.Join(home, "config")
	dataHome = filepath.Join(home, "data")
	t.Setenv("HOME", home)
	// Windows resolves the home directory from USERPROFILE, not HOME.
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	for _, k := range []string{"SAL_CONFIG", "SAL_SESSION_DIR", "SAL_MODEL", "SAL_BASE_URL", "SAL_API_KEY", "OPENAI_MODEL", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_API_BASE", "OPENCODE_GO_API_KEY", "NAMED_KEY"} {
		os.Unsetenv(k)
		t.Setenv(k, "")
	}
	return home, cfgHome, dataHome
}

func writeConfig(t *testing.T, cfgHome, name, content string) string {
	t.Helper()
	dir := filepath.Join(cfgHome, "sal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureStdout runs fn with os.Stdout redirected to a pipe.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = orig
	data, _ := io.ReadAll(r)
	return string(data), runErr
}

func TestResolvePrecedenceFlagBeatsEnvBeatsConfig(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	path := writeConfig(t, cfgHome, "config.toml", `
model = "from-config"
base_url = "http://config.example/v1"
tools = "read,grep"
max_turns = 9
`)
	t.Setenv("SAL_MODEL", "from-env")

	opts, set, err := parseArgs([]string{"--model", "from-flag"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("", config.Resolve())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != path {
		t.Fatalf("config not found: %q", cfg.Path)
	}
	resolved, sources := resolveOptions(opts, set, cfg)

	if resolved.model != "from-flag" {
		t.Fatalf("flag should win: model = %q", resolved.model)
	}
	if resolved.baseURL != "http://config.example/v1" {
		t.Fatalf("config should supply base_url: %q", resolved.baseURL)
	}
	if resolved.toolList != "read,grep" {
		t.Fatalf("config should supply tools: %q", resolved.toolList)
	}
	if resolved.maxTurns != 9 {
		t.Fatalf("config should supply max_turns: %d", resolved.maxTurns)
	}

	src := map[string]string{}
	for _, s := range sources {
		src[s.Key] = s.Source
	}
	if src["model"] != "flag" {
		t.Fatalf("model source = %q, want flag", src["model"])
	}
	if !strings.HasPrefix(src["base_url"], "config ") {
		t.Fatalf("base_url source = %q, want config", src["base_url"])
	}
	if src["api_key"] != "default" {
		t.Fatalf("api_key source = %q, want default", src["api_key"])
	}
}

func TestEnvBeatsConfigWhenNoFlag(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	writeConfig(t, cfgHome, "config.toml", `model = "from-config"`)
	t.Setenv("SAL_MODEL", "from-env")

	opts, set, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load("", config.Resolve())
	resolved, sources := resolveOptions(opts, set, cfg)

	if resolved.model != "from-env" {
		t.Fatalf("env should beat config: model = %q", resolved.model)
	}
	for _, s := range sources {
		if s.Key == "model" && s.Source != "env $SAL_MODEL" {
			t.Fatalf("model source = %q", s.Source)
		}
	}
}

// A flag set to its zero value must still beat the config file, which is why
// flag presence is tracked rather than flag value.
func TestExplicitZeroValueFlagStillWins(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	writeConfig(t, cfgHome, "config.toml", "no_session = true\nmax_turns = 9\n")

	opts, set, err := parseArgs([]string{"--max-turns", "0"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load("", config.Resolve())
	resolved, _ := resolveOptions(opts, set, cfg)

	if resolved.maxTurns != 0 {
		t.Fatalf("explicit --max-turns 0 should override config: got %d", resolved.maxTurns)
	}
	if !resolved.noSession {
		t.Fatal("no_session from config should still apply")
	}
}

// The whole point: a config file alone must be enough to drive a real run.
func TestConfigFileDrivesARealRun(t *testing.T) {
	_, cfgHome, dataHome := isolateXDG(t)
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{
		toolChunks("c1", "write", `{"path":"from-config.txt","content":"config works\n"}`, ``),
		textChunks("Wrote it using the config file."),
	})
	writeConfig(t, cfgHome, "config.toml", `
model = "config-model"
base_url = "`+mock.server.URL+`"
api_key = "config-key"
no_session = true
tools = "write,read"
`)

	oldWd, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	stdout, err := captureStdout(t, func() error {
		return runWithArgs([]string{"-p", "write the file"})
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if !strings.Contains(stdout, "Wrote it using the config file.") {
		t.Fatalf("stdout missing the assistant text: %q", stdout)
	}

	// the tool ran against the working directory
	data, err := os.ReadFile(filepath.Join(workdir, "from-config.txt"))
	if err != nil {
		t.Fatalf("tool from the configured run did not write the file: %v", err)
	}
	if string(data) != "config works\n" {
		t.Fatalf("wrong content: %q", data)
	}

	// only the configured tools were advertised
	declared, _ := mock.requests[0]["tools"].([]any)
	if len(declared) != 2 {
		t.Fatalf("expected the config tool allowlist to apply, got %d tools", len(declared))
	}
	// the configured model was used
	if mock.requests[0]["model"] != "config-model" {
		t.Fatalf("configured model not used: %v", mock.requests[0]["model"])
	}
	// no session directory should have been created thanks to no_session
	if entries, err := os.ReadDir(filepath.Join(dataHome, "sal")); err == nil && len(entries) > 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no_session should prevent session files, found %v", names)
	}
}

func TestShowConfigReportsPathsSourcesAndMasksSecrets(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	writeConfig(t, cfgHome, "config.toml", `
model = "shown-model"
api_key = "sk-supersecretvalue"
`)
	t.Setenv("SAL_MODEL", "env-model")

	out, err := captureStdout(t, func() error {
		return runWithArgs([]string{"--show-config"})
	})
	if err != nil {
		t.Fatalf("--show-config failed: %v", err)
	}

	if !strings.Contains(out, filepath.Join(cfgHome, "sal", "config.toml")) {
		t.Fatalf("config path missing:\n%s", out)
	}
	if !strings.Contains(out, "env-model") || !strings.Contains(out, "env $SAL_MODEL") {
		t.Fatalf("resolved model and its source missing:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(cfgHome, "sal", "sessions")) &&
		!strings.Contains(out, filepath.Join(cfgHome, "data", "sal")) {
		// session dir comes from XDG_DATA_HOME
		if !strings.Contains(out, "session dir") {
			t.Fatalf("session dir missing:\n%s", out)
		}
	}
	if strings.Contains(out, "supersecretvalue") {
		t.Fatalf("api key must be masked:\n%s", out)
	}
	if !strings.Contains(out, "masked") {
		t.Fatalf("expected a masked marker for the api key:\n%s", out)
	}
}

func TestSessionsUseXDGDataDirectory(t *testing.T) {
	_, cfgHome, dataHome := isolateXDG(t)
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{textChunks("hello back")})

	writeConfig(t, cfgHome, "config.toml", `
model = "m"
base_url = "`+mock.server.URL+`"
api_key = "k"
`)

	oldWd, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	if _, err := captureStdout(t, func() error {
		return runWithArgs([]string{"-p", "hi"})
	}); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	root := filepath.Join(dataHome, "sal", "sessions")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("sessions should live under $XDG_DATA_HOME: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no session bucket created in %s: %v", root, err)
	}
	bucket := filepath.Join(root, entries[0].Name())
	files, _ := os.ReadDir(bucket)
	if len(files) != 1 || !strings.HasSuffix(files[0].Name(), ".jsonl") {
		t.Fatalf("expected one session file in %s, got %v", bucket, files)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".sal")); err == nil {
		t.Fatal("nothing should be written outside the XDG directories")
	}
}

func TestValidateResolvedRejectsUnknownProviderFromConfig(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	writeConfig(t, cfgHome, "config.toml", `provider = "anthropic"`)

	err := runWithArgs([]string{"-p", "hi"})
	if err == nil {
		t.Fatal("expected an error for an unsupported provider in the config file")
	}
	if !strings.Contains(err.Error(), "anthropic") {
		t.Fatalf("error should name the provider: %v", err)
	}
}

// TestListSessionsShowsTheConversationsForTheDirectory covers the CLI half of
// session switching: the ids printed here are what --session accepts.
// TestListSessionsShowsEveryDirectory pins the machine-wide listing: one line
// per directory, because a directory has one conversation.
func TestListSessionsShowsEveryDirectory(t *testing.T) {
	isolateXDG(t)
	workdir := t.TempDir()
	other := t.TempDir()

	current, resumed, err := session.Open(session.RootWith(""), workdir, "the first task", "m")
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Fatal("a fresh directory must not resume")
	}
	_ = current.AppendMessage(agent.NewUserMessage("hello"))
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}

	elsewhere, err := session.New(session.RootWith(""), other, "another project", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = elsewhere.Close()

	out, err := captureStdout(t, func() error {
		return runWithArgs([]string{"--list-sessions"})
	})
	if err != nil {
		t.Fatalf("--list-sessions failed: %v", err)
	}
	if !strings.Contains(out, "the first task") {
		t.Fatalf("the stored name should be listed:\n%s", out)
	}
	if !strings.Contains(out, "another project") {
		t.Fatalf("every directory should be listed:\n%s", out)
	}
	if !strings.Contains(out, "--dir") {
		t.Fatalf("the listing should say how to reach a conversation:\n%s", out)
	}
	// One line per conversation, not one per file.
	if strings.Count(out, "conversation(s) on this machine") != 1 {
		t.Fatalf("unexpected listing:\n%s", out)
	}
}

// TestDirSelectsAnotherConversation covers --dir: the named directory's
// conversation is resumed, which is what replaced --continue-all.
func TestDirSelectsAnotherConversation(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	other := t.TempDir()
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{textChunks("hello back")})

	writeConfig(t, cfgHome, "config.toml", `
model = "m"
base_url = "`+mock.server.URL+`"
api_key = "k"
`)

	// A conversation recorded under a different working directory...
	stored, _, err := session.Open(session.RootWith(""), other, "older", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := stored.AppendMessage(agent.NewUserMessage("what did we decide?")); err != nil {
		t.Fatal(err)
	}
	if err := stored.Close(); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	if _, err := captureStdout(t, func() error {
		return runWithArgs([]string{"--dir", other, "-p", "carry on"})
	}); err != nil {
		t.Fatalf("--dir failed: %v", err)
	}

	// The resumed run must have sent the stored history, not start from zero.
	if len(mock.requests) == 0 {
		t.Fatal("no request reached the provider")
	}
	msgs, _ := mock.requests[0]["messages"].([]any)
	var carried bool
	for _, raw := range msgs {
		if m, ok := raw.(map[string]any); ok && m["content"] == "what did we decide?" {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("the stored message is missing from the request: %+v", msgs)
	}
}

// TestNewKeepsThePreviousConversation covers --new: the stored file is moved
// aside and a fresh conversation begins.
func TestNewKeepsThePreviousConversation(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	workdir := t.TempDir()
	mock := newMockServer(t, [][]string{textChunks("fresh start")})

	writeConfig(t, cfgHome, "config.toml", `
model = "m"
base_url = "`+mock.server.URL+`"
api_key = "k"
`)

	stored, _, err := session.Open(session.RootWith(""), workdir, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = stored.AppendMessage(agent.NewUserMessage("old question"))
	oldPath := stored.Path
	if err := stored.Close(); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	if err := os.Chdir(workdir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	if _, err := captureStdout(t, func() error {
		return runWithArgs([]string{"--new", "-p", "a new question"})
	}); err != nil {
		t.Fatalf("--new failed: %v", err)
	}

	// The request must not carry the previous conversation.
	if len(mock.requests) == 0 {
		t.Fatal("no request reached the provider")
	}
	msgs, _ := mock.requests[0]["messages"].([]any)
	for _, raw := range msgs {
		if m, ok := raw.(map[string]any); ok && m["content"] == "old question" {
			t.Fatalf("--new carried the previous conversation: %+v", msgs)
		}
	}
	// The previous conversation is kept under an archive name, and the new file
	// holds only the new turn.
	entries, err := os.ReadDir(filepath.Dir(oldPath))
	if err != nil {
		t.Fatal(err)
	}
	var archived string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "session-") {
			archived = filepath.Join(filepath.Dir(oldPath), e.Name())
		}
	}
	if archived == "" {
		t.Fatalf("the previous conversation was not kept: %v", entries)
	}
	kept, err := session.Load(archived)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.Messages) == 0 || kept.Messages[0].Content != "old question" {
		t.Fatalf("the archived conversation lost its messages: %+v", kept.Messages)
	}
	fresh, err := session.Load(session.Path(session.RootWith(""), workdir))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range fresh.Messages {
		if m.Content == "old question" {
			t.Fatalf("the new conversation carries the old one: %+v", fresh.Messages)
		}
	}
}

// TestMaxRetriesPrecedence covers the retry budget resolution: flag > config >
// default, with 0 meaning "use the default".
func TestMaxRetriesPrecedence(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)

	// Default when nothing is configured.
	o, set, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("", config.Resolve())
	if err != nil {
		t.Fatal(err)
	}
	resolved, sources := resolveOptions(o, set, cfg)
	if resolved.maxRetries != agent.DefaultMaxRetries {
		t.Fatalf("default max retries = %d", resolved.maxRetries)
	}
	if !hasSetting(sources, "max_retries", "default") {
		t.Fatalf("source not reported: %+v", sources)
	}

	// From the config file.
	writeConfig(t, cfgHome, "config.toml", "max_retries = 7\n")
	cfg, err = config.Load("", config.Resolve())
	if err != nil {
		t.Fatal(err)
	}
	o, set, _ = parseArgs(nil)
	resolved, sources = resolveOptions(o, set, cfg)
	if resolved.maxRetries != 7 {
		t.Fatalf("config max retries = %d, want 7", resolved.maxRetries)
	}
	if !hasSetting(sources, "max_retries", "config") {
		t.Fatalf("source not reported: %+v", sources)
	}

	// The flag wins, including a negative value that disables retrying.
	o, set, _ = parseArgs([]string{"--max-retries", "-1"})
	resolved, _ = resolveOptions(o, set, cfg)
	if resolved.maxRetries != -1 {
		t.Fatalf("flag max retries = %d, want -1", resolved.maxRetries)
	}
}

// hasSetting reports whether a source entry for key mentions the given origin.
func hasSetting(sources []settingSource, key, origin string) bool {
	for _, s := range sources {
		if s.Key == key && strings.Contains(s.Source, origin) {
			return true
		}
	}
	return false
}

// TestNegativeMaxRetriesFromConfig covers the "never retry" setting surviving a
// config-file round trip, which the 0-means-default rule could easily swallow.
func TestNegativeMaxRetriesFromConfig(t *testing.T) {
	_, cfgHome, _ := isolateXDG(t)
	writeConfig(t, cfgHome, "config.toml", "max_retries = -1\n")

	cfg, err := config.Load("", config.Resolve())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.File.MaxRetries != -1 {
		t.Fatalf("config parsed max_retries = %d, want -1", cfg.File.MaxRetries)
	}
	o, set, err := parseArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, sources := resolveOptions(o, set, cfg)
	if resolved.maxRetries != -1 {
		t.Fatalf("resolved max retries = %d, want -1", resolved.maxRetries)
	}
	if !hasSetting(sources, "max_retries", "config") {
		t.Fatalf("source not reported: %+v", sources)
	}
}
