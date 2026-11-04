// Package config loads sal's configuration file and resolves its storage
// locations following the XDG Base Directory Specification.
//
// Layout:
//
//	$XDG_CONFIG_HOME/sal/config.toml   (or config.json)   user configuration
//	$XDG_DATA_HOME/sal/sessions/                          session files
//	$XDG_STATE_HOME/sal/web-token                         web console token
//
// XDG_CONFIG_HOME defaults to ~/.config, XDG_DATA_HOME to ~/.local/share and
// XDG_STATE_HOME to ~/.local/state. Relative values in those variables are
// ignored, as the specification requires.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Paths holds sal's resolved directories.
type Paths struct {
	ConfigDir string // $XDG_CONFIG_HOME/sal
	DataDir   string // $XDG_DATA_HOME/sal
	StateDir  string // $XDG_STATE_HOME/sal
	// SessionDir is the XDG location for session files.
	SessionDir string
}

// Resolve computes the directories from the environment.
func Resolve() Paths {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	cfgHome := xdgPath("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dataHome := xdgPath("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	stateHome := xdgPath("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	return Paths{
		ConfigDir:  filepath.Join(cfgHome, "sal"),
		DataDir:    filepath.Join(dataHome, "sal"),
		StateDir:   filepath.Join(stateHome, "sal"),
		SessionDir: filepath.Join(dataHome, "sal", "sessions"),
	}
}

// xdgPath returns an XDG base directory, ignoring relative values (invalid per
// the specification) and falling back to the default.
func xdgPath(env, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" && filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	return fallback
}

// ConfigPath returns the config file location, overridable with $SAL_CONFIG.
func (p Paths) ConfigPath() string {
	if v := strings.TrimSpace(os.Getenv("SAL_CONFIG")); v != "" {
		return v
	}
	return filepath.Join(p.ConfigDir, "config.toml")
}

// CandidatePaths lists the config files that are searched, in order.
func (p Paths) CandidatePaths() []string {
	return []string{
		filepath.Join(p.ConfigDir, "config.toml"),
		filepath.Join(p.ConfigDir, "config.json"),
	}
}

// File is the configuration schema. TOML uses snake_case keys, matching the
// JSON field names.
type File struct {
	// Provider is accepted for forward compatibility; only "openai" is bundled.
	Provider string `toml:"provider" json:"provider"`
	Model    string `toml:"model" json:"model"`
	BaseURL  string `toml:"base_url" json:"base_url"`
	// APIKey works but the SAL_API_KEY environment variable is preferred, so
	// credentials stay out of dotfiles.
	APIKey string `toml:"api_key" json:"api_key"`
	// APIKeyEnv names an environment variable to read the key from instead of
	// storing it here, e.g. "GATEWAY_API_KEY". Checked before the generic
	// SAL_API_KEY / OPENAI_API_KEY variables.
	APIKeyEnv string `toml:"api_key_env" json:"api_key_env"`
	// Tools is a comma-separated allowlist of built-in tools.
	Tools string `toml:"tools" json:"tools"`
	// MaxTurns caps agent turns per prompt (0 = built-in default).
	MaxTurns int `toml:"max_turns" json:"max_turns"`
	// MaxRetries is how many times a transient provider failure is retried
	// (0 = built-in default, negative disables retrying).
	MaxRetries int `toml:"max_retries" json:"max_retries"`
	// ContextWindow overrides the model's context window in tokens. 0 resolves
	// it from the endpoint, a built-in table, or a conservative default. A "k"
	// or "m" suffix is accepted, so 128k and 128000 are the same.
	ContextWindow Size `toml:"context_window" json:"context_window"`
	// AutoCompact compacts older messages when the context window fills.
	// Unset means on.
	AutoCompact *bool `toml:"auto_compact" json:"auto_compact"`
	// CompactAt is the percentage of the window at which automatic compaction
	// runs (0 = built-in default).
	CompactAt int `toml:"compact_at" json:"compact_at"`
	// SystemPrompt replaces the default system prompt; a value that names an
	// existing file is read from disk.
	SystemPrompt string `toml:"system_prompt" json:"system_prompt"`
	// AppendSystemPrompt is added after the default prompt and context files.
	AppendSystemPrompt string `toml:"append_system_prompt" json:"append_system_prompt"`
	// SessionDir overrides the XDG session location.
	SessionDir string `toml:"session_dir" json:"session_dir"`
	// SessionHeader names the request header carrying the per-conversation
	// session id. Empty means "auto": a header is sent only for gateways known
	// to want one (e.g. opencode.ai wants x-opencode-session).
	SessionHeader string `toml:"session_header" json:"session_header"`
	// Name sets a default session display name.
	Name string `toml:"name" json:"name"`
	// Pointers so "unset" is distinguishable from false.
	NoSession      *bool `toml:"no_session" json:"no_session"`
	NoContextFiles *bool `toml:"no_context_files" json:"no_context_files"`
}

// Size is a token count that may be written with a k/m suffix ("128k", "1m"),
// because that is how model context windows are usually quoted. It is an int,
// so a bare number works too.
type Size int

// UnmarshalText implements encoding.TextUnmarshaler, which is how the TOML
// decoder reads a quoted value into it.
func (s *Size) UnmarshalText(b []byte) error {
	n, err := ParseSize(string(b))
	if err != nil {
		return err
	}
	*s = Size(n)
	return nil
}

// UnmarshalJSON accepts both 128000 and "128k".
func (s *Size) UnmarshalJSON(b []byte) error {
	text := strings.TrimSpace(string(b))
	if text == "null" || text == "" {
		return nil
	}
	text = strings.Trim(text, `"`)
	n, err := ParseSize(text)
	if err != nil {
		return err
	}
	*s = Size(n)
	return nil
}

// ParseSize reads a token count, with an optional k/m suffix (case-insensitive).
func ParseSize(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	mult := 1
	switch last := v[len(v)-1]; last {
	case 'k', 'K':
		mult, v = 1000, v[:len(v)-1]
	case 'm', 'M':
		mult, v = 1000000, v[:len(v)-1]
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("invalid token count %q: want a number, optionally with a k or m suffix", v)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid token count %d: must not be negative", n)
	}
	return n * mult, nil
}

// Loaded is a parsed configuration file plus its provenance.
type Loaded struct {
	File File
	// Path is empty when no configuration file was found.
	Path string
	// Warnings are non-fatal problems worth surfacing to the user.
	Warnings []string
}

// Load reads the configuration file. An explicit path must exist; otherwise the
// first existing candidate is used, and a missing file is not an error.
func Load(explicit string, p Paths) (Loaded, error) {
	if strings.TrimSpace(explicit) != "" {
		l, err := loadFile(explicit, true)
		if err != nil {
			return Loaded{}, err
		}
		return l, nil
	}
	for _, candidate := range p.CandidatePaths() {
		st, err := os.Stat(candidate)
		if err != nil || st.IsDir() {
			continue
		}
		return loadFile(candidate, false)
	}
	return Loaded{}, nil
}

func loadFile(path string, required bool) (Loaded, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if required {
			return Loaded{}, fmt.Errorf("cannot read config file %s: %w", path, err)
		}
		return Loaded{}, nil
	}
	var f File
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		dec := json.NewDecoder(strings.NewReader(string(data)))
		if err := dec.Decode(&f); err != nil {
			return Loaded{}, fmt.Errorf("config file %s is not valid JSON: %w", path, err)
		}
	default:
		md, err := toml.Decode(string(data), &f)
		if err != nil {
			return Loaded{}, fmt.Errorf("config file %s is not valid TOML: %w", path, err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, 0, len(undecoded))
			for _, k := range undecoded {
				keys = append(keys, k.String())
			}
			l := Loaded{File: f, Path: path}
			l.Warnings = append(l.Warnings,
				fmt.Sprintf("%s: ignoring unknown key(s): %s", path, strings.Join(keys, ", ")))
			return l, nil
		}
	}

	l := Loaded{File: f, Path: path}
	if f.APIKey != "" {
		if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
			l.Warnings = append(l.Warnings, fmt.Sprintf(
				"%s is readable by other users (%04o) but contains an api_key; prefer the SAL_API_KEY environment variable or chmod 600",
				path, st.Mode().Perm()))
		}
	}
	return l, nil
}

// SessionRootName documents the subdirectory used under the data directory.
const SessionRootName = "sessions"

// ExpandPath expands a leading "~" to the user's home directory. Values in
// config files are written by hand, so "~/..." must work.
func ExpandPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p[0] != '~' {
		return p
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p // e.g. "~otheruser", which is not ours to resolve
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
}
