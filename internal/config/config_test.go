package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withEnv sets environment variables for the duration of a test. HOME is
// redirected so tests never touch the real dotfiles.
func withEnv(t *testing.T, home string, kv map[string]string) {
	t.Helper()
	t.Setenv("HOME", home)
	// Windows resolves the home directory from USERPROFILE, not HOME.
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "SAL_CONFIG", "SAL_SESSION_DIR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestResolveDefaultsToDotPaths(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)

	p := Resolve()
	if want := filepath.Join(home, ".config", "sal"); p.ConfigDir != want {
		t.Fatalf("ConfigDir = %s, want %s", p.ConfigDir, want)
	}
	if want := filepath.Join(home, ".local", "share", "sal", "sessions"); p.SessionDir != want {
		t.Fatalf("SessionDir = %s, want %s", p.SessionDir, want)
	}
	if want := filepath.Join(home, ".local", "state", "sal"); p.StateDir != want {
		t.Fatalf("StateDir = %s, want %s", p.StateDir, want)
	}
}

func TestResolveHonorsXDGVariables(t *testing.T) {
	home := t.TempDir()
	cfgHome := filepath.Join(home, "xdg-config")
	dataHome := filepath.Join(home, "xdg-data")
	stateHome := filepath.Join(home, "xdg-state")
	withEnv(t, home, map[string]string{
		"XDG_CONFIG_HOME": cfgHome,
		"XDG_DATA_HOME":   dataHome,
		"XDG_STATE_HOME":  stateHome,
	})

	p := Resolve()
	if p.ConfigDir != filepath.Join(cfgHome, "sal") {
		t.Fatalf("ConfigDir = %s", p.ConfigDir)
	}
	if p.DataDir != filepath.Join(dataHome, "sal") {
		t.Fatalf("DataDir = %s", p.DataDir)
	}
	if p.SessionDir != filepath.Join(dataHome, "sal", "sessions") {
		t.Fatalf("SessionDir = %s", p.SessionDir)
	}
	if p.StateDir != filepath.Join(stateHome, "sal") {
		t.Fatalf("StateDir = %s", p.StateDir)
	}
}

// The XDG spec says relative paths in the XDG_* variables are invalid and must
// be ignored.
func TestResolveIgnoresRelativeXDGPaths(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, map[string]string{
		"XDG_CONFIG_HOME": "relative/config",
		"XDG_DATA_HOME":   "./data",
	})
	p := Resolve()
	if p.ConfigDir != filepath.Join(home, ".config", "sal") {
		t.Fatalf("relative XDG_CONFIG_HOME should be ignored, got %s", p.ConfigDir)
	}
	if p.DataDir != filepath.Join(home, ".local", "share", "sal") {
		t.Fatalf("relative XDG_DATA_HOME should be ignored, got %s", p.DataDir)
	}
}

func TestLoadTOML(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	content := fmt.Sprintf(`
# sal configuration
model = "gpt-4o"
base_url = "https://example.test/v1"
tools = "read,grep"
max_turns = 7
no_context_files = true
session_dir = %q
`, sessionDir)
	if err := os.WriteFile(filepath.Join(p.ConfigDir, "config.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path == "" {
		t.Fatal("config file not found")
	}
	f := cfg.File
	if f.Model != "gpt-4o" || f.BaseURL != "https://example.test/v1" || f.Tools != "read,grep" {
		t.Fatalf("strings not parsed: %+v", f)
	}
	if f.MaxTurns != 7 {
		t.Fatalf("max_turns = %d", f.MaxTurns)
	}
	if f.NoContextFiles == nil || !*f.NoContextFiles {
		t.Fatal("no_context_files = true not parsed")
	}
	if f.SessionDir != sessionDir {
		t.Fatalf("session_dir = %q", f.SessionDir)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", cfg.Warnings)
	}
}

func TestLoadJSON(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"model":"claude-x","max_turns":3,"no_session":false}`
	if err := os.WriteFile(filepath.Join(p.ConfigDir, "config.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.File.Model != "claude-x" || cfg.File.MaxTurns != 3 {
		t.Fatalf("json not parsed: %+v", cfg.File)
	}
	if cfg.File.NoSession == nil || *cfg.File.NoSession {
		t.Fatal("no_session should be parsed as false")
	}
}

// TOML normally wins over JSON in the same directory.
func TestTOMLBeatsJSON(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.ConfigDir, "config.toml"), []byte(`model = "from-toml"`), 0o600)
	os.WriteFile(filepath.Join(p.ConfigDir, "config.json"), []byte(`{"model":"from-json"}`), 0o600)

	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.File.Model != "from-toml" {
		t.Fatalf("expected config.toml to win, got %q", cfg.File.Model)
	}
}

func TestExplicitPathWinsAndMissingPathIsAnError(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.ConfigDir, "config.toml"), []byte(`model = "from-xdg"`), 0o600)

	other := filepath.Join(home, "custom.toml")
	os.WriteFile(other, []byte(`model = "from-custom"`), 0o600)

	cfg, err := Load(other, p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.File.Model != "from-custom" || cfg.Path != other {
		t.Fatalf("explicit path not honored: %+v", cfg)
	}

	if _, err := Load(filepath.Join(home, "does-not-exist.toml"), p); err == nil {
		t.Fatal("an explicit missing config file must be an error")
	}
}

func TestMissingConfigIsNotAnError(t *testing.T) {
	withEnv(t, t.TempDir(), nil)
	cfg, err := Load("", Resolve())
	if err != nil {
		t.Fatalf("a missing config file must not error: %v", err)
	}
	if cfg.Path != "" || len(cfg.Warnings) != 0 {
		t.Fatalf("expected an empty config, got %+v", cfg)
	}
}

func TestBrokenConfigReportsPath(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	os.MkdirAll(p.ConfigDir, 0o755)
	bad := filepath.Join(p.ConfigDir, "config.toml")
	os.WriteFile(bad, []byte("model = "), 0o600)

	_, err := Load("", p)
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Fatalf("error should name the file: %v", err)
	}
}

func TestUnknownKeysAreWarnedAbout(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	os.MkdirAll(p.ConfigDir, 0o755)
	os.WriteFile(filepath.Join(p.ConfigDir, "config.toml"), []byte("model = \"m\"\nmodle = \"typo\"\n"), 0o600)

	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) == 0 || !strings.Contains(cfg.Warnings[0], "modle") {
		t.Fatalf("expected a warning about the unknown key, got %v", cfg.Warnings)
	}
}

func TestWorldReadableConfigWithKeyWarns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not carry POSIX permission bits, so the check cannot fire")
	}
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()
	os.MkdirAll(p.ConfigDir, 0o755)
	path := filepath.Join(p.ConfigDir, "config.toml")
	os.WriteFile(path, []byte("api_key = \"sk-abcdefghijklmnop\"\n"), 0o600)
	// chmod explicitly: the ambient umask (0077 here) would otherwise strip the
	// group/other bits and the warning could never trigger.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "readable by other users") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a permissions warning, got %v", cfg.Warnings)
	}

	// tightening the permissions silences it
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load("", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "readable by other users") {
			t.Fatal("600 permissions should not warn")
		}
	}
}

func TestSessionDirsFollowXDG(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)
	p := Resolve()

	// the canonical session location is always under XDG_DATA_HOME
	if want := filepath.Join(home, ".local", "share", "sal", "sessions"); p.SessionDir != want {
		t.Fatalf("SessionDir = %s, want %s", p.SessionDir, want)
	}
	// creating a ~/.sal directory must not move the canonical location
	if err := os.MkdirAll(filepath.Join(home, ".sal", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Resolve().SessionDir; got != p.SessionDir {
		t.Fatalf("SessionDir moved: %s", got)
	}
}

func TestConfigPathEnvOverride(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, map[string]string{"SAL_CONFIG": "/etc/sal/custom.toml"})
	if got := Resolve().ConfigPath(); got != "/etc/sal/custom.toml" {
		t.Fatalf("ConfigPath = %s", got)
	}
}

func TestExpandPath(t *testing.T) {
	home := t.TempDir()
	withEnv(t, home, nil)

	cases := map[string]string{
		"":              "",
		"/abs/path":     "/abs/path",
		"relative/path": "relative/path",
		"~":             home,
		"~/sessions":    filepath.Join(home, "sessions"),
		"~/a/b/c":       filepath.Join(home, "a", "b", "c"),
		"~otheruser/x":  "~otheruser/x", // not ours to resolve
	}
	for in, want := range cases {
		if got := ExpandPath(in); got != want {
			t.Fatalf("ExpandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSizeAcceptsSuffixes(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"128k", 128000},
		{"1m", 1000000},
		{"128K", 128000},
		{"  64k ", 64000},
		{"32000", 32000},
		{"", 0},
	} {
		got, err := ParseSize(c.in)
		if err != nil || got != c.want {
			t.Fatalf("ParseSize(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"abc", "-1k", "12x"} {
		if _, err := ParseSize(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestContextSettingsLoadFromBothFormats(t *testing.T) {
	dir := t.TempDir()
	paths := Paths{ConfigDir: dir}

	tomlPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(
		"context_window = \"200k\"\nauto_compact = false\ncompact_at = 65\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load("", paths)
	if err != nil {
		t.Fatal(err)
	}
	if int(l.File.ContextWindow) != 200000 {
		t.Fatalf("quoted size in TOML: %d", int(l.File.ContextWindow))
	}
	if l.File.AutoCompact == nil || *l.File.AutoCompact {
		t.Fatal("auto_compact = false should be recorded")
	}
	if l.File.CompactAt != 65 {
		t.Fatalf("compact_at = %d", l.File.CompactAt)
	}

	jsonDir := t.TempDir()
	jsonPath := filepath.Join(jsonDir, "config.json")
	if err := os.WriteFile(jsonPath, []byte(
		`{"context_window":"1m","auto_compact":true,"compact_at":70}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err = Load("", Paths{ConfigDir: jsonDir})
	if err != nil {
		t.Fatal(err)
	}
	if int(l.File.ContextWindow) != 1000000 {
		t.Fatalf("string size in JSON: %d", int(l.File.ContextWindow))
	}
}

func TestSizeAcceptsPlainNumbersInJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"context_window": 32000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load("", Paths{ConfigDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if int(l.File.ContextWindow) != 32000 {
		t.Fatalf("numeric size: %d", int(l.File.ContextWindow))
	}
}
