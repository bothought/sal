// Package upgrade replaces the running sal binary with the newest release
// published on GitHub. It is deliberately independent of the rest of the
// program: an upgrade needs no provider, no session and no terminal, and it
// must stay usable when the agent is not.
package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Repository is the project whose releases carry the binaries.
const (
	RepoOwner = "bothought"
	RepoName  = "sal"
)

// DefaultAPIBase is the GitHub API the release is looked up at.
const DefaultAPIBase = "https://api.github.com"

// Limits for a download. A release archive is a few tens of megabytes at most.
const (
	defaultMaxDownload = 256 << 20
	defaultTimeout     = 5 * time.Minute
	// testTimeout bounds the version check run on the freshly unpacked binary.
	testTimeout = 20 * time.Second
)

// Options configures one upgrade run. The entry point fills in the real values;
// the tests fill in a local server and a temporary executable.
type Options struct {
	// Version is the installed version, as printed by --version.
	Version string
	// GOOS and GOARCH default to the running machine.
	GOOS, GOARCH string
	// ExePath is the binary to replace; defaults to the running executable.
	ExePath string
	// APIBase defaults to DefaultAPIBase.
	APIBase string
	// HTTPClient defaults to a client with no overall timeout (the download is
	// bounded by context and by size instead).
	HTTPClient *http.Client
	// Out receives progress messages; nil discards them.
	Out io.Writer
	// Check reports whether an update exists without changing anything.
	Check bool
	// MaxDownload bounds the archive size; 0 uses the built-in limit.
	MaxDownload int64
	// Timeout bounds the whole run; 0 uses the built-in limit.
	Timeout time.Duration
}

type options struct {
	Options
	client *http.Client
	out    io.Writer
}

// target is the archive form this machine needs.
type target struct {
	// label is the platform as the release names it ("linux/arm64").
	label string
	// suffix matches the asset for this platform ("_linux_arm64.tar.gz").
	suffix string
	// zip selects the packaging format; the release uses zip on Windows.
	zip bool
	// binary is the member name inside the archive.
	binary string
}

// platformFor maps a machine to the archive the release publishes for it.
// Android runs Linux binaries its architecture matches, so it maps onto the
// Linux archives; an unknown platform is an error rather than a guess.
func platformFor(goos, goarch string) (target, error) {
	arch := goarch
	switch goos {
	case "linux", "android":
		if arch != "amd64" && arch != "arm64" {
			return target{}, fmt.Errorf("no release asset for %s/%s", goos, goarch)
		}
		goos = "linux"
	case "darwin":
		if arch != "amd64" && arch != "arm64" {
			return target{}, fmt.Errorf("no release asset for darwin/%s", goarch)
		}
	case "windows":
		if arch != "amd64" && arch != "arm64" {
			return target{}, fmt.Errorf("no release asset for windows/%s", goarch)
		}
	default:
		return target{}, fmt.Errorf("no release asset for %s/%s", goos, goarch)
	}
	t := target{label: goos + "/" + arch, binary: "sal"}
	if goos == "windows" {
		t.suffix = "_windows_" + arch + ".zip"
		t.zip = true
		t.binary = "sal.exe"
		return t, nil
	}
	t.suffix = "_" + goos + "_" + arch + ".tar.gz"
	return t, nil
}

// Run performs (or, with Check, only reports) an upgrade.
func Run(ctx context.Context, opts Options) error {
	if opts.MaxDownload == 0 {
		opts.MaxDownload = defaultMaxDownload
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}
	o := options{Options: opts, out: opts.Out}
	if o.out == nil {
		o.out = io.Discard
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if o.APIBase == "" {
		o.APIBase = DefaultAPIBase
	}
	if o.HTTPClient != nil {
		o.client = o.HTTPClient
	} else {
		o.client = newHTTPClient()
	}

	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	platform, err := platformFor(o.GOOS, o.GOARCH)
	if err != nil {
		return err
	}

	rel, err := fetchRelease(ctx, o)
	if err != nil {
		return err
	}
	latest, latestOK := parseVersion(rel.Tag)
	if !latestOK {
		return fmt.Errorf("the latest release tag %q is not a semantic version", rel.Tag)
	}
	installed, installedOK := parseVersion(o.Version)

	if o.Check {
		return reportCheck(o.out, o.Version, rel, installed, installedOK, latest)
	}
	if installedOK && installed.compare(latest) >= 0 {
		reportCurrent(o.out, o.Version, installed, latest, installedOK)
		return nil
	}

	exe, err := executablePath(o.ExePath)
	if err != nil {
		return err
	}
	asset, err := selectAsset(rel, platform)
	if err != nil {
		return err
	}
	checksums, err := selectChecksums(rel)
	if err != nil {
		return err
	}

	fmt.Fprintf(o.out, "sal %s → %s (%s)\n", o.Version, latest.string(), platform.label)
	dir := filepath.Dir(exe)
	archive, sum, err := download(ctx, o, asset, dir)
	if err != nil {
		return err
	}
	defer os.Remove(archive)

	fmt.Fprint(o.out, "verifying checksum ... ")
	if err := verifyChecksum(ctx, o, checksums, asset.Name, sum); err != nil {
		fmt.Fprintln(o.out, "failed")
		return err
	}
	fmt.Fprintln(o.out, "ok")

	newFile, err := extract(archive, platform, dir)
	if err != nil {
		return err
	}
	defer os.Remove(newFile)

	if err := testRun(ctx, newFile, rel.Tag); err != nil {
		return err
	}

	fmt.Fprintf(o.out, "replacing %s ... ", exe)
	if err := replaceExecutable(exe, newFile); err != nil {
		fmt.Fprintln(o.out, "failed")
		return err
	}
	fmt.Fprintln(o.out, "ok")
	fmt.Fprintf(o.out, "sal upgraded to %s; start sal again to use the new binary\n", latest.string())
	return nil
}

// reportCheck prints what --check reports and never touches the filesystem.
func reportCheck(w io.Writer, installedRaw string, rel *release, installed version, installedOK bool, latest version) error {
	fmt.Fprintf(w, "sal %s\n", installedRaw)
	fmt.Fprintf(w, "latest release: %s\n", latest.string())
	switch {
	case !installedOK:
		fmt.Fprintf(w, "installed version %q cannot be compared; run 'sal upgrade' to install the latest release\n", installedRaw)
	case installed.compare(latest) < 0:
		fmt.Fprintf(w, "an update is available: %s → %s\n", installed.string(), latest.string())
		if rel.Page != "" {
			fmt.Fprintf(w, "  %s\n", rel.Page)
		}
		fmt.Fprintln(w, "  run 'sal upgrade' to install it")
	case installed.compare(latest) == 0:
		fmt.Fprintf(w, "sal %s is up to date\n", installed.string())
	default:
		fmt.Fprintf(w, "sal %s is newer than the latest release (%s); nothing to do\n", installed.string(), latest.string())
	}
	return nil
}

// reportCurrent prints the same conclusion --check would for a run that has
// nothing to install.
func reportCurrent(w io.Writer, installedRaw string, installed, latest version, installedOK bool) {
	fmt.Fprintf(w, "sal %s\n", installedRaw)
	fmt.Fprintf(w, "latest release: %s\n", latest.string())
	if !installedOK {
		// Unreachable: a run without a comparable version always upgrades.
		fmt.Fprintf(w, "installed version %q cannot be compared\n", installedRaw)
		return
	}
	if installed.compare(latest) > 0 {
		fmt.Fprintf(w, "sal %s is newer than the latest release (%s); nothing to do\n", installed.string(), latest.string())
		return
	}
	fmt.Fprintf(w, "sal %s is up to date\n", installed.string())
}

// selectAsset picks the one archive for the platform.
func selectAsset(rel *release, p target) (releaseAsset, error) {
	var found []releaseAsset
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, p.suffix) {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return releaseAsset{}, fmt.Errorf("the release %s has no asset for %s (expected one matching *%s)", rel.Tag, p.label, p.suffix)
	}
	names := make([]string, len(found))
	for i, a := range found {
		names[i] = a.Name
	}
	return releaseAsset{}, fmt.Errorf("the release %s has %d assets for %s (%s); refusing to guess",
		rel.Tag, len(found), p.label, strings.Join(names, ", "))
}

// selectChecksums picks the release's checksum document.
func selectChecksums(rel *release) (releaseAsset, error) {
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, "_checksums.txt") {
			return a, nil
		}
	}
	return releaseAsset{}, fmt.Errorf("the release %s has no checksum file; refusing to install an unverified binary", rel.Tag)
}

// download fetches an asset into a temporary file in dir, returning its path and
// the SHA-256 of what was written.
func download(ctx context.Context, o options, asset releaseAsset, dir string) (string, string, error) {
	u, err := url.Parse(asset.URL)
	if err != nil {
		return "", "", fmt.Errorf("cannot parse the download URL %q: %w", asset.URL, err)
	}
	if !secureURL(u) {
		return "", "", fmt.Errorf("refusing to download %s over %s", asset.Name, u.Scheme)
	}

	fmt.Fprintf(o.out, "downloading %s (%s) ... ", asset.Name, humanSize(asset.Size))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", fmt.Errorf("cannot build the download request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent(o.Version))
	resp, err := o.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("cannot download %s: %w", asset.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("downloading %s answered HTTP %d", asset.Name, resp.StatusCode)
	}

	f, err := os.CreateTemp(dir, ".sal-upgrade-*.archive")
	if err != nil {
		return "", "", fmt.Errorf("cannot create a temporary file in %s: %w", dir, err)
	}
	path := f.Name()
	cleanup := func(err error) (string, string, error) {
		f.Close()
		os.Remove(path)
		return "", "", err
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, o.MaxDownload+1))
	if err != nil {
		return cleanup(fmt.Errorf("cannot download %s: %w", asset.Name, err))
	}
	if n > o.MaxDownload {
		return cleanup(fmt.Errorf("%s is larger than the %s download limit", asset.Name, humanSize(o.MaxDownload)))
	}
	if err := f.Sync(); err != nil {
		return cleanup(fmt.Errorf("cannot write the temporary file: %w", err))
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", "", fmt.Errorf("cannot write the temporary file: %w", err)
	}
	fmt.Fprintln(o.out, "done")
	return path, hex.EncodeToString(h.Sum(nil)), nil
}

// verifyChecksum fetches the release's checksum document and compares it with
// the digest of what was downloaded.
func verifyChecksum(ctx context.Context, o options, checksums releaseAsset, assetName, sum string) error {
	u, err := url.Parse(checksums.URL)
	if err != nil {
		return fmt.Errorf("cannot parse the checksum URL %q: %w", checksums.URL, err)
	}
	if !secureURL(u) {
		return fmt.Errorf("refusing to download %s over %s", checksums.Name, u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("cannot build the checksum request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent(o.Version))
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot download %s: %w", checksums.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s answered HTTP %d", checksums.Name, resp.StatusCode)
	}

	want, err := checksumFor(io.LimitReader(resp.Body, 1<<20), assetName)
	if err != nil {
		return err
	}
	if !strings.EqualFold(want, sum) {
		return fmt.Errorf("%s failed verification: the download does not match the release checksum; nothing was replaced", assetName)
	}
	return nil
}

// checksumFor reads a sha256sum-format document and returns the digest for one
// file name.
func checksumFor(r io.Reader, name string) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		listed := strings.TrimPrefix(strings.TrimSpace(fields[len(fields)-1]), "*")
		if listed != name {
			continue
		}
		if len(fields[0]) != 64 {
			return "", fmt.Errorf("the checksum for %s in the checksum file is malformed", name)
		}
		return fields[0], nil
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("cannot read the checksum file: %w", err)
	}
	return "", fmt.Errorf("the checksum file does not mention %s; refusing to install it", name)
}

// extract unpacks the platform binary from the archive into a temporary file in
// dir, with the mode an executable needs.
func extract(archive string, p target, dir string) (string, error) {
	body, err := os.Open(archive)
	if err != nil {
		return "", fmt.Errorf("cannot open the downloaded archive: %w", err)
	}
	defer body.Close()

	var member io.Reader
	if p.zip {
		data, err := io.ReadAll(body)
		if err != nil {
			return "", fmt.Errorf("cannot read the downloaded archive: %w", err)
		}
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return "", fmt.Errorf("the downloaded file is not a valid zip archive: %w", err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
			if filepath.Base(f.Name) != p.binary {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("cannot read %s inside the archive: %w", f.Name, err)
			}
			defer rc.Close()
			member = rc
			break
		}
		if member == nil {
			return "", fmt.Errorf("the archive does not contain %s (found %s)", p.binary, strings.Join(names, ", "))
		}
	} else {
		gz, err := gzip.NewReader(body)
		if err != nil {
			return "", fmt.Errorf("the downloaded file is not a valid gzip archive: %w", err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		var names []string
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", fmt.Errorf("cannot read the downloaded archive: %w", err)
			}
			names = append(names, hdr.Name)
			if filepath.Base(hdr.Name) == p.binary && hdr.Typeflag == tar.TypeReg {
				member = tr
				break
			}
		}
		if member == nil {
			return "", fmt.Errorf("the archive does not contain %s (found %s)", p.binary, strings.Join(names, ", "))
		}
	}

	f, err := os.CreateTemp(dir, ".sal-upgrade-*.bin")
	if err != nil {
		return "", fmt.Errorf("cannot create a temporary file in %s: %w", dir, err)
	}
	path := f.Name()
	_, werr := io.Copy(f, io.LimitReader(member, defaultMaxDownload))
	if werr == nil {
		werr = f.Sync()
	}
	if err := f.Close(); err != nil && werr == nil {
		werr = err
	}
	if werr != nil {
		os.Remove(path)
		return "", fmt.Errorf("cannot unpack %s: %w", p.binary, werr)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("cannot make the unpacked binary executable: %w", err)
	}
	return path, nil
}

// testRun executes the freshly unpacked binary and requires it to report the
// release version. It is the only execution of downloaded code, and it happens
// before the installed binary is touched.
func testRun(ctx context.Context, path, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// A minimal environment: the binary needs none of what started sal, and an
	// inherited variable that happens to be malformed must not fail the check.
	env := []string{"PATH=" + os.Getenv("PATH")}
	for _, name := range []string{"HOME", "TMPDIR", "TEMP", "TMP", "SystemRoot", "windir", "PATHEXT"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = ": " + truncate(detail, 200)
		}
		return fmt.Errorf("the downloaded binary does not run (%v%s); nothing was replaced", err, detail)
	}
	got := strings.TrimSpace(string(out))
	want := strings.TrimPrefix(tag, "v")
	if !strings.Contains(got, want) {
		return fmt.Errorf("the downloaded binary reports %q instead of %s; nothing was replaced", truncate(got, 80), tag)
	}
	return nil
}

// executablePath resolves what to replace, following symbolic links so an
// upgrade keeps the name the binary is invoked by.
func executablePath(explicit string) (string, error) {
	path := explicit
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot locate the running binary: %w", err)
		}
		path = exe
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s: %w", abs, err)
	}
	return resolved, nil
}

// CleanupStale removes files an earlier upgrade left behind. It is a no-op
// outside Windows, where the running executable can be replaced in place; on
// Windows the previous binary cannot be deleted until its process exits, so it
// is removed on the next start. Failures are ignored: a stale file costs disk,
// not correctness.
func CleanupStale() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	cleanupOldBinaries(exe)
}

// humanSize formats a byte count the way the progress lines show it.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// string renders a version the way the output shows it, without the leading v.
func (v version) string() string {
	s := fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
	if v.pre != "" {
		s += "-" + v.pre
	}
	return s
}
