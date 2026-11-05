package main

import (
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upgradeServer answers the release endpoint with one release and an empty
// asset list; the CLI tests assert on the report, not on an install.
func upgradeServer(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"` + tag + `","html_url":"https://github.com/bothought/sal/releases/tag/` + tag + `","assets":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useUpgradeServer points the command at a local server for one test.
func useUpgradeServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := upgradeAPIBase
	upgradeAPIBase = srv.URL
	t.Cleanup(func() { upgradeAPIBase = orig })
}

// TestUpgradeCheckReportsAnAvailableUpdate covers `sal upgrade --check`.
func TestUpgradeCheckReportsAnAvailableUpdate(t *testing.T) {
	srv := upgradeServer(t, "v99.0.0")
	useUpgradeServer(t, srv)
	origVersion := version
	version = "0.1.0"
	t.Cleanup(func() { version = origVersion })

	out, err := captureStdout(t, func() error {
		return runWithArgs([]string{"upgrade", "--check"})
	})
	if err != nil {
		t.Fatalf("runWithArgs: %v", err)
	}
	for _, want := range []string{"sal 0.1.0", "latest release: 99.0.0", "an update is available: 0.1.0 → 99.0.0", "run 'sal upgrade'"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not contain %q:\n%s", want, out)
		}
	}
}

// TestUpgradeCheckNeedsNoConfiguration proves the subcommand runs before the
// configuration file is loaded and before credentials are resolved.
func TestUpgradeCheckNeedsNoConfiguration(t *testing.T) {
	srv := upgradeServer(t, "v0.1.0")
	useUpgradeServer(t, srv)
	_, cfgHome, _ := isolateXDG(t)
	if err := os.MkdirAll(filepath.Join(cfgHome, "sal"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A config file that would fail any other command: an unknown key is a
	// warning, but a bad provider is an error.
	if err := os.WriteFile(filepath.Join(cfgHome, "sal", "config.toml"),
		[]byte("provider = \"anthropic\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAL_API_KEY", "")
	origVersion := version
	version = "0.1.0"
	t.Cleanup(func() { version = origVersion })

	out, err := captureStdout(t, func() error {
		return runWithArgs([]string{"upgrade", "--check"})
	})
	if err != nil {
		t.Fatalf("upgrade --check must not depend on the config file: %v", err)
	}
	if !strings.Contains(out, "is up to date") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

// TestUpgradeIsNotAPromptWord checks the escape hatch: with `--` the word is a
// prompt again, so the subcommand cannot swallow a request by accident.
func TestUpgradeIsNotAPromptWord(t *testing.T) {
	if handled, _ := runSubcommand([]string{"--", "upgrade"}); handled {
		t.Fatal("`sal -- upgrade` must not be read as the subcommand")
	}
	opts, _, err := parseArgs([]string{"--", "upgrade", "the", "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opts.prompts, " "); got != "upgrade the docs" {
		t.Fatalf("prompt = %q, want %q", got, "upgrade the docs")
	}
}

func TestUpgradeRejectsExtraArguments(t *testing.T) {
	handled, err := runSubcommand([]string{"upgrade", "--check", "extra"})
	if !handled {
		t.Fatal("the upgrade word must be handled")
	}
	if err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Fatalf("extra arguments: %v", err)
	}
}

func TestUpgradeHelp(t *testing.T) {
	// The flag package writes usage to stderr and returns flag.ErrHelp, which
	// main treats as a normal exit.
	handled, err := runSubcommand([]string{"upgrade", "-h"})
	if !handled {
		t.Fatal("the upgrade word must be handled")
	}
	if err == nil || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("upgrade -h = %v, want flag.ErrHelp", err)
	}
}

// TestUpgradeReportsAFailedDownload covers the error path through the command:
// the message reaches the caller, so main prints one `sal: ...` line.
func TestUpgradeReportsAFailedDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	useUpgradeServer(t, srv)
	origVersion := version
	version = "0.1.0"
	t.Cleanup(func() { version = origVersion })

	_, err := captureStdout(t, func() error {
		return runWithArgs([]string{"upgrade", "--check"})
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("a failing endpoint must be reported: %v", err)
	}
}

// TestVersionHasABuildFallback keeps the installed version meaningful for every
// way sal is built.
func TestVersionHasABuildFallback(t *testing.T) {
	if version == "" {
		t.Fatal("version must never be empty")
	}
	// The test binary is a development build: it must not claim a release.
	if version != "dev" {
		t.Fatalf("a plain go test binary reported %q, want dev", version)
	}
}

// TestInjectedVersionWins covers the release-build path.
func TestInjectedVersionWins(t *testing.T) {
	orig := injectedVersion
	injectedVersion = "1.2.3"
	t.Cleanup(func() { injectedVersion = orig })
	if got := buildVersion(); got != "1.2.3" {
		t.Fatalf("buildVersion = %q, want 1.2.3", got)
	}
}

func TestPseudoVersionsAreDevelopmentBuilds(t *testing.T) {
	pseudo := []string{
		"0.1.1-0.20261105023111-2661c2422dcd",
		"v0.1.1-0.20261105023111-2661c2422dcd+dirty",
		"1.2.3-0.20260101000000-abcdef123456",
		"1.2.3-rc.1.0.20260101000000-abcdef123456",
	}
	for _, v := range pseudo {
		if !isPseudoVersion(v) {
			t.Fatalf("%q must be treated as a pseudo-version", v)
		}
	}
	releases := []string{"0.1.0", "v1.2.3", "1.2.3-rc.1", "1.2.3-beta2", ""}
	for _, v := range releases {
		if isPseudoVersion(v) {
			t.Fatalf("%q must not be treated as a pseudo-version", v)
		}
	}
}
