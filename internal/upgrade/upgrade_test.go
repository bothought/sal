package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		ok   bool
		want string
	}{
		{"0.1.0", true, "0.1.0"},
		{"v0.2.0", true, "0.2.0"},
		{"1.10.3", true, "1.10.3"},
		{"1.0.0-rc.1", true, "1.0.0-rc.1"},
		{"1.0.0+build.5", true, "1.0.0"},
		{"dev", false, ""},
		{"(devel)", false, ""},
		{"", false, ""},
		{"1.0", false, ""},
		{"1.0.0.0", false, ""},
		{"v", false, ""},
		{"1.0.x", false, ""},
	}
	for _, tt := range tests {
		got, ok := parseVersion(tt.in)
		if ok != tt.ok {
			t.Fatalf("parseVersion(%q) ok = %v, want %v", tt.in, ok, tt.ok)
		}
		if ok && got.string() != tt.want {
			t.Fatalf("parseVersion(%q) = %q, want %q", tt.in, got.string(), tt.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	order := []string{"0.1.0", "0.1.1", "0.2.0", "0.10.0", "1.0.0-rc.1", "1.0.0", "1.0.1"}
	for i := 0; i < len(order); i++ {
		for j := 0; j < len(order); j++ {
			a, _ := parseVersion(order[i])
			b, _ := parseVersion(order[j])
			got := a.compare(b)
			want := compareInt(i, j)
			if got != want {
				t.Fatalf("%s.compare(%s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	// A prerelease sorts below the release with the same numbers.
	rc, _ := parseVersion("1.0.0-rc.1")
	rel, _ := parseVersion("1.0.0")
	if rc.compare(rel) >= 0 {
		t.Fatalf("1.0.0-rc.1 should sort before 1.0.0")
	}
}

func TestPlatformFor(t *testing.T) {
	tests := []struct {
		goos, goarch string
		label        string
		suffix       string
		zip          bool
		binary       string
		wantErr      bool
	}{
		{"linux", "amd64", "linux/amd64", "_linux_amd64.tar.gz", false, "sal", false},
		{"linux", "arm64", "linux/arm64", "_linux_arm64.tar.gz", false, "sal", false},
		{"android", "arm64", "android/arm64", "_android_arm64.tar.gz", false, "sal", false},
		{"darwin", "arm64", "darwin/arm64", "_darwin_arm64.tar.gz", false, "sal", false},
		{"windows", "amd64", "windows/amd64", "_windows_amd64.zip", true, "sal.exe", false},
		{"windows", "arm64", "windows/arm64", "_windows_arm64.zip", true, "sal.exe", false},
		{"linux", "386", "", "", false, "", true},
		{"android", "amd64", "", "", false, "", true},
		{"plan9", "arm64", "", "", false, "", true},
		{"freebsd", "amd64", "", "", false, "", true},
	}
	for _, tt := range tests {
		got, err := platformFor(tt.goos, tt.goarch)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("platformFor(%s/%s) = %v, want an error", tt.goos, tt.goarch, got)
			}
			// The error must name the platform, so a user knows what is missing.
			if !strings.Contains(err.Error(), tt.goos) {
				t.Fatalf("platformFor(%s/%s) error %q does not name the platform", tt.goos, tt.goarch, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("platformFor(%s/%s): %v", tt.goos, tt.goarch, err)
		}
		if got.label != tt.label || got.suffix != tt.suffix || got.zip != tt.zip || got.binary != tt.binary {
			t.Fatalf("platformFor(%s/%s) = %+v, want label %q suffix %q zip %v binary %q",
				tt.goos, tt.goarch, got, tt.label, tt.suffix, tt.zip, tt.binary)
		}
	}
}

func TestSelectAsset(t *testing.T) {
	p, err := platformFor("linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	one := &release{Tag: "v0.2.0", Assets: []releaseAsset{
		{Name: "sal_0.2.0_darwin_arm64.tar.gz"},
		{Name: "sal_0.2.0_linux_arm64.tar.gz"},
		{Name: "sal_0.2.0_checksums.txt"},
	}}
	got, err := selectAsset(one, p)
	if err != nil || got.Name != "sal_0.2.0_linux_arm64.tar.gz" {
		t.Fatalf("selectAsset = %+v, %v", got, err)
	}

	none := &release{Tag: "v0.2.0", Assets: []releaseAsset{{Name: "sal_0.2.0_darwin_arm64.tar.gz"}}}
	if _, err := selectAsset(none, p); err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Fatalf("selectAsset without a match: %v", err)
	}

	two := &release{Tag: "v0.2.0", Assets: []releaseAsset{
		{Name: "a_linux_arm64.tar.gz"},
		{Name: "b_linux_arm64.tar.gz"},
	}}
	if _, err := selectAsset(two, p); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("selectAsset with two matches: %v", err)
	}
}

func TestSelectChecksums(t *testing.T) {
	if _, err := selectChecksums(&release{Tag: "v0.2.0"}); err == nil {
		t.Fatal("a release without checksums must be refused")
	}
	rel := &release{Tag: "v0.2.0", Assets: []releaseAsset{{Name: "x_checksums.txt"}}}
	if _, err := selectChecksums(rel); err != nil {
		t.Fatalf("selectChecksums: %v", err)
	}
}

func TestChecksumFor(t *testing.T) {
	doc := "aaa" + strings.Repeat("0", 61) + "  sal_0.2.0_linux_arm64.tar.gz\n" +
		strings.Repeat("b", 64) + " *sal_0.2.0_darwin_arm64.tar.gz\n"
	got, err := checksumFor(strings.NewReader(doc), "sal_0.2.0_darwin_arm64.tar.gz")
	if err != nil || got != strings.Repeat("b", 64) {
		t.Fatalf("checksumFor = %q, %v", got, err)
	}
	if _, err := checksumFor(strings.NewReader(doc), "missing.tar.gz"); err == nil {
		t.Fatal("a missing entry must be an error")
	}
}

// fakeRelease serves a release description, an archive and a checksum file.
type fakeRelease struct {
	server    *httptest.Server
	assetName string
	asset     []byte
	checksum  string
	tag       string
}

func newFakeRelease(t *testing.T, p target, script string) *fakeRelease {
	t.Helper()
	f := &fakeRelease{tag: "v9.9.9", assetName: "sal_9.9.9" + p.suffix}
	var buf bytes.Buffer
	if p.zip {
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(p.binary)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(script))
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: p.binary, Mode: 0o755, Size: int64(len(script))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(script))
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	f.asset = buf.Bytes()
	sum := sha256.Sum256(f.asset)
	f.checksum = hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+RepoOwner+"/"+RepoName+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name": f.tag,
			"html_url": "https://github.com/" + RepoOwner + "/" + RepoName + "/releases/tag/" + f.tag,
			"assets": []map[string]any{
				{"name": f.assetName, "browser_download_url": f.server.URL + "/dl/asset", "size": len(f.asset)},
				{"name": "sal_9.9.9_checksums.txt", "browser_download_url": f.server.URL + "/dl/checksums"},
			},
		})
	})
	mux.HandleFunc("/dl/asset", func(w http.ResponseWriter, r *http.Request) { w.Write(f.asset) })
	mux.HandleFunc("/dl/checksums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", f.checksum, f.assetName)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// hostTarget returns the platform the test host upgrades for, or skips when the
// suite runs on something the release does not build for.
func hostTarget(t *testing.T) target {
	t.Helper()
	p, err := platformFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("the release does not build for %s/%s: %v", runtime.GOOS, runtime.GOARCH, err)
	}
	return p
}

// executableScript is a stand-in binary that reports the release version. It
// needs a POSIX shell, so the tests that execute it are skipped on Windows.
func executableScript(version string) string {
	return "#!/bin/sh\necho sal " + version + "\n"
}

func TestUpgradeReplacesTheBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	p := hostTarget(t)
	f := newFakeRelease(t, p, executableScript("9.9.9"))

	dir := t.TempDir()
	exe := filepath.Join(dir, "sal")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := Run(context.Background(), Options{
		Version:    "0.1.0",
		ExePath:    exe,
		APIBase:    f.server.URL,
		HTTPClient: f.server.Client(),
		Out:        &out,
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != executableScript("9.9.9") {
		t.Fatalf("the binary was not replaced: %q", body)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the replaced binary is not executable: %v", info.Mode())
	}
	if !strings.Contains(out.String(), "sal upgraded to 9.9.9") {
		t.Fatalf("the progress output does not report the new version:\n%s", out.String())
	}
	// No temporary files survive a successful run.
	assertNoTempFiles(t, dir)
}

// TestUpgradeFollowsSymlink checks that an upgrade replaces the file a link
// points to and leaves the link where it is.
func TestUpgradeFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	p := hostTarget(t)
	f := newFakeRelease(t, p, executableScript("9.9.9"))

	dir := t.TempDir()
	real := filepath.Join(dir, "sal-real")
	if err := os.WriteFile(real, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "sal")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}

	if err := Run(context.Background(), Options{
		Version:    "0.1.0",
		ExePath:    link,
		APIBase:    f.server.URL,
		HTTPClient: f.server.Client(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if target, err := os.Readlink(link); err != nil || target != real {
		t.Fatalf("the link was not preserved: %q, %v", target, err)
	}
	body, _ := os.ReadFile(real)
	if string(body) != executableScript("9.9.9") {
		t.Fatalf("the file behind the link was not replaced: %q", body)
	}
}

func TestCheckWritesNothing(t *testing.T) {
	p := hostTarget(t)
	f := newFakeRelease(t, p, executableScript("9.9.9"))

	dir := t.TempDir()
	exe := filepath.Join(dir, "sal")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := Run(context.Background(), Options{
		Version:    "0.1.0",
		ExePath:    exe,
		APIBase:    f.server.URL,
		HTTPClient: f.server.Client(),
		Out:        &out,
		Check:      true,
	})
	if err != nil {
		t.Fatalf("Run --check: %v", err)
	}
	if want := "an update is available: 0.1.0 → 9.9.9"; !strings.Contains(out.String(), want) {
		t.Fatalf("output does not contain %q:\n%s", want, out.String())
	}
	if body, _ := os.ReadFile(exe); string(body) != "old binary" {
		t.Fatalf("--check changed the binary: %q", body)
	}
	assertNoTempFiles(t, dir)
}

func TestUpgradeRefusesBadChecksum(t *testing.T) {
	p := hostTarget(t)
	f := newFakeRelease(t, p, executableScript("9.9.9"))
	f.checksum = strings.Repeat("0", 64)

	dir := t.TempDir()
	exe := filepath.Join(dir, "sal")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{
		Version:    "0.1.0",
		ExePath:    exe,
		APIBase:    f.server.URL,
		HTTPClient: f.server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("Run with a bad checksum: %v", err)
	}
	if body, _ := os.ReadFile(exe); string(body) != "old binary" {
		t.Fatalf("a failed upgrade changed the binary: %q", body)
	}
	assertNoTempFiles(t, dir)
}

func TestUpgradeRejectsWrongVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test binary is a shell script")
	}
	p := hostTarget(t)
	// The archive is well formed and checksummed, but its binary is not the
	// release the tag names.
	f := newFakeRelease(t, p, executableScript("0.0.1"))

	dir := t.TempDir()
	exe := filepath.Join(dir, "sal")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{
		Version:    "0.1.0",
		ExePath:    exe,
		APIBase:    f.server.URL,
		HTTPClient: f.server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "nothing was replaced") {
		t.Fatalf("Run with a mismatched binary: %v", err)
	}
	if body, _ := os.ReadFile(exe); string(body) != "old binary" {
		t.Fatalf("a rejected upgrade changed the binary: %q", body)
	}
	assertNoTempFiles(t, dir)
}

func TestUpgradeReportsRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	err := Run(context.Background(), Options{
		Version:    "0.1.0",
		ExePath:    filepath.Join(t.TempDir(), "sal"),
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("Run against a rate-limited API: %v", err)
	}
}

func TestSecureURL(t *testing.T) {
	allowed := []string{"https://api.github.com/x", "https://example.com/x", "http://127.0.0.1:8080/x", "http://localhost/x", "http://[::1]:9/x"}
	refused := []string{"http://example.com/x", "ftp://example.com/x", "file:///etc/passwd", ""}
	for _, raw := range allowed {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("cannot parse %q: %v", raw, err)
		}
		if !secureURL(u) {
			t.Fatalf("%q must be allowed", raw)
		}
	}
	for _, raw := range refused {
		u, err := url.Parse(raw)
		if err != nil {
			continue // an unparseable URL cannot be fetched either
		}
		if secureURL(u) {
			t.Fatalf("%q must be refused", raw)
		}
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sal-upgrade-") {
			t.Fatalf("temporary file %s was left behind", e.Name())
		}
	}
}

// TestUpgradeIsSkippedWhenCurrent covers the "nothing to do" path of an upgrade.
func TestUpgradeIsSkippedWhenCurrent(t *testing.T) {
	p := hostTarget(t)
	f := newFakeRelease(t, p, executableScript("9.9.9"))
	f.tag = "v0.1.0"

	dir := t.TempDir()
	exe := filepath.Join(dir, "sal")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), Options{
		Version:    "0.1.0",
		ExePath:    exe,
		APIBase:    f.server.URL,
		HTTPClient: f.server.Client(),
		Out:        &out,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("output does not say it is up to date:\n%s", out.String())
	}
	if body, _ := os.ReadFile(exe); string(body) != "old binary" {
		t.Fatalf("a no-op upgrade changed the binary: %q", body)
	}
}

func TestRunTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	err := Run(context.Background(), Options{
		Version:    "0.1.0",
		ExePath:    filepath.Join(t.TempDir(), "sal"),
		APIBase:    srv.URL,
		HTTPClient: srv.Client(),
		Timeout:    100 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a run past its deadline must fail")
	}
}

func TestHumanSize(t *testing.T) {
	tests := map[int64]string{0: "0 B", 999: "999 B", 1024: "1.0 KiB", 11*1024*1024 + 400*1024: "11.4 MiB"}
	for n, want := range tests {
		if got := humanSize(n); got != want {
			t.Fatalf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestAndroidIsNotEighteenLinux pins the reason the platform table keeps Android
// separate. An Android device runs Linux binaries, so folding the two looks
// harmless, but a Linux build of sal looks for /etc/resolv.conf while an Android
// build reads the resolver file Termux installs. Installing the Linux archive on
// Android therefore produces a binary that starts, reports its version, and
// cannot resolve a name — which also breaks every later upgrade.
func TestAndroidIsNotLinux(t *testing.T) {
	android, err := platformFor("android", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	linux, err := platformFor("linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if android.suffix == linux.suffix {
		t.Fatalf("android must not share the Linux asset (%s)", android.suffix)
	}
	if !strings.Contains(android.suffix, "_android_") {
		t.Fatalf("android asset = %q, want an android archive", android.suffix)
	}
	// The release must be able to satisfy it, so the platform set is checked
	// against the naming rule the pipeline uses.
	if android.label != "android/arm64" {
		t.Fatalf("android label = %q", android.label)
	}
}
