package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNameserverIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	body := "# leading comment\n;another\n\nnameserver 8.8.8.8\nnameserver 1.1.1.1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := nameserverIn(path); got != "8.8.8.8:53" {
		t.Fatalf("nameserverIn = %q, want 8.8.8.8:53", got)
	}
	if got := nameserverIn(filepath.Join(dir, "missing")); got != "" {
		t.Fatalf("a missing file must yield no nameserver, got %q", got)
	}
	// A non-address must be skipped rather than handed to a dialer.
	if got := nameserverIn(writeResolv(t, "nameserver not-an-ip\nnameserver 9.9.9.9\n")); got != "9.9.9.9:53" {
		t.Fatalf("a malformed nameserver must be skipped, got %q", got)
	}
}

func writeResolv(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestNoFallbackWhenTheSystemFileExists pins that the workaround never runs on
// an ordinary machine: the system file, when present, is what is used.
func TestNoFallbackWhenTheSystemFileExists(t *testing.T) {
	if _, err := os.Stat(systemResolverFile); err != nil {
		t.Skip("this host has no " + systemResolverFile + ", so the fallback is the active path")
	}
	if got := findNameserver(); got != "" {
		t.Fatalf("findNameserver = %q on a host with %s; the fallback must not run", got, systemResolverFile)
	}
}

// TestRootCAsOnlyStepsInWhenTheSystemHasNone pins that the certificate
// workaround is inert on an ordinary host, and that an explicit bundle wins.
func TestRootCAsOnlyStepsInWhenTheSystemHasNone(t *testing.T) {
	if os.Getenv("SSL_CERT_FILE") != "" {
		t.Skip("the environment names a bundle; the system choice must win")
	}
	system := false
	for _, p := range systemCertFiles {
		if _, err := os.Stat(p); err == nil {
			system = true
			break
		}
	}
	got := rootCAs()
	if system && got != nil {
		t.Fatalf("rootCAs returned a pool on a host with a system bundle; the workaround must not run")
	}
	if !system && got == nil {
		t.Skip("this host has neither a system bundle nor an alternate one")
	}
}

// TestNameserverInIsBounded keeps a hostile resolver file from being read
// forever; a nameserver line is short by definition.
func TestNameserverInIsBounded(t *testing.T) {
	long := "nameserver " + strings.Repeat("9", 100000) + "\nnameserver 1.1.1.1\n"
	if got := nameserverIn(writeResolv(t, long)); got != "1.1.1.1:53" {
		t.Fatalf("a malformed long line must be skipped, got %q", got)
	}
}
