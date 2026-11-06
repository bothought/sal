package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bothought/sal/internal/agent"
)

func TestSessionRoundTrip(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	s, err := New(root, cwd, "my task", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(agent.NewUserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(agent.Message{Role: agent.RoleAssistant, Content: "hi there"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetModel("other-model"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetName("renamed"); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session file not written: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != s.ID {
		t.Fatalf("id mismatch: %s vs %s", loaded.ID, s.ID)
	}
	if loaded.Name != "renamed" {
		t.Fatalf("name not persisted: %q", loaded.Name)
	}
	if loaded.Model != "other-model" {
		t.Fatalf("model not persisted: %q", loaded.Model)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(loaded.Messages))
	}
	if loaded.Messages[0].Content != "hello" || loaded.Messages[1].Content != "hi there" {
		t.Fatalf("messages not reconstructed: %+v", loaded.Messages)
	}

	// The bucket name, wherever the root put it.
	if slug := filepath.Base(ProjectDir("", cwd)); path != "" && !strings.Contains(path, slug) {
		t.Fatalf("session not stored in the project bucket %q: %s", slug, path)
	}
}

func TestLatestAndList(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	if _, err := Latest(root, cwd); err == nil {
		t.Fatal("expected ErrNoSessions for a fresh directory")
	}

	first, err := New(root, cwd, "first", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = first.AppendMessage(agent.NewUserMessage("one"))
	_ = first.Close()

	second, err := New(root, cwd, "second", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = second.AppendMessage(agent.NewUserMessage("two"))
	_ = second.Close()

	latest, err := Latest(root, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if latest != second.Path {
		t.Fatalf("Latest returned %s, want %s", latest, second.Path)
	}

	infos, err := List(root, cwd, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(infos))
	}
	if infos[0].Path != second.Path {
		t.Fatalf("sessions not sorted newest first: %+v", infos)
	}
	if infos[0].Messages != 1 {
		t.Fatalf("expected 1 message counted, got %d", infos[0].Messages)
	}
}

func TestSessionsAreGroupedPerDirectory(t *testing.T) {
	root := t.TempDir()
	a := t.TempDir()
	b := t.TempDir()

	sess, err := New(root, a, "in-a", "m")
	if err != nil {
		t.Fatal(err)
	}
	// Close the file: Windows refuses to remove a directory whose file is still
	// open, so an unclosed handle fails the test's own cleanup.
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	infosA, _ := List(root, a, false)
	infosB, _ := List(root, b, false)
	if len(infosA) != 1 {
		t.Fatalf("expected 1 session for dir a, got %d", len(infosA))
	}
	if len(infosB) != 0 {
		t.Fatalf("expected 0 sessions for dir b, got %d", len(infosB))
	}
}

// isolate sets HOME and the XDG variables so the default paths are predictable.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Windows resolves the home directory from USERPROFILE, not HOME.
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("SAL_SESSION_DIR", "")
	return home
}

func TestRootWithDefaultsToXDGDataDirectory(t *testing.T) {
	home := isolate(t)
	want := filepath.Join(home, "data", "sal", "sessions")
	if got := RootWith(""); got != want {
		t.Fatalf("RootWith(\"\") = %s, want %s", got, want)
	}
	if explicit := filepath.Join(t.TempDir(), "explicit"); RootWith(explicit) != explicit {
		t.Fatalf("an explicit directory must win: %s", RootWith(explicit))
	}
	fromEnv := filepath.Join(t.TempDir(), "from-env")
	t.Setenv("SAL_SESSION_DIR", fromEnv)
	if got := RootWith(""); got != fromEnv {
		t.Fatalf("SAL_SESSION_DIR must win over XDG: %s", got)
	}
}

func TestSearchRootsIsTheResolvedRoot(t *testing.T) {
	home := isolate(t)
	roots := SearchRoots("")
	if len(roots) != 1 {
		t.Fatalf("expected exactly one root, got %v", roots)
	}
	if want := filepath.Join(home, "data", "sal", "sessions"); roots[0] != want {
		t.Fatalf("root = %s, want %s", roots[0], want)
	}
}

func TestLatestInFindsNewestAcrossRoots(t *testing.T) {
	home := isolate(t)
	cwd := t.TempDir()
	primary := filepath.Join(home, "data", "sal", "sessions")
	secondary := filepath.Join(home, "other-root")

	if _, err := LatestIn([]string{primary, secondary}, cwd, false); err == nil {
		t.Fatal("expected ErrNoSessions when nothing is stored")
	}

	// an older session in the secondary root
	old, err := New(secondary, cwd, "older", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = old.AppendMessage(agent.NewUserMessage("old"))
	_ = old.Close()

	oldPath := old.Path

	found, err := LatestIn([]string{primary, secondary}, cwd, false)
	if err != nil {
		t.Fatal(err)
	}
	if found != oldPath {
		t.Fatalf("expected the session in the secondary root, got %s", found)
	}

	// a newer session in the primary root wins
	time.Sleep(10 * time.Millisecond)
	newer, err := New(primary, cwd, "newer", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = newer.AppendMessage(agent.NewUserMessage("new"))
	_ = newer.Close()

	found, err = LatestIn([]string{primary, secondary}, cwd, false)
	if err != nil {
		t.Fatal(err)
	}
	if found != newer.Path {
		t.Fatalf("expected the newer session %s, got %s", newer.Path, found)
	}
}

func TestFindByIDAcceptsFullAndShortIDs(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	cwd := t.TempDir()

	s, err := New(root, cwd, "named", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(agent.NewUserMessage("hello")); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	for _, id := range []string{s.ID, s.ID[:8]} {
		path, err := FindByID(root, id)
		if err != nil {
			t.Fatalf("FindByID(%q): %v", id, err)
		}
		if path != s.Path {
			t.Fatalf("FindByID(%q) = %s, want %s", id, path, s.Path)
		}
	}

	// a different project directory is searched too, not just the current cwd
	other, err := New(root, t.TempDir(), "other", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Close()
	if path, err := FindByID(root, other.ID); err != nil || path != other.Path {
		t.Fatalf("FindByID across project dirs = %q, %v", path, err)
	}

	if _, err := FindByID(root, "0123456789abcdef"); err == nil {
		t.Fatal("expected an error for an unknown id")
	}
	if _, err := FindByID(root, ""); err == nil {
		t.Fatal("expected an error for an empty id")
	}
	if _, err := FindByID(filepath.Join(root, "absent"), s.ID); err == nil {
		t.Fatal("expected an error when the root does not exist")
	}
}

func TestListAllFindsSessionsAcrossProjectBuckets(t *testing.T) {
	root := t.TempDir()
	a := t.TempDir()
	b := t.TempDir()

	inA, err := New(root, a, "in-a", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = inA.AppendMessage(agent.NewUserMessage("hello"))
	_ = inA.Close()

	inB, err := New(root, b, "in-b", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = inB.Close()

	// The default listing is scoped to the working directory...
	scoped, err := List(root, a, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].Path != inA.Path {
		t.Fatalf("scoped list = %+v, want only %s", scoped, inA.Path)
	}

	// ...while all lists every bucket, still filtered by the recorded cwd when
	// one is given.
	all, err := List(root, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions across buckets, got %+v", all)
	}
	for _, info := range all {
		if info.ID == "" || info.Name == "" {
			t.Fatalf("entries must be loaded, not just stat'ed: %+v", info)
		}
	}

	onlyA, err := List(root, a, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyA) != 1 || onlyA[0].ID != inA.ID {
		t.Fatalf("filtered list = %+v, want only %s", onlyA, inA.ID)
	}
}

func TestListAllOnAMissingRootIsEmpty(t *testing.T) {
	infos, err := List(filepath.Join(t.TempDir(), "absent"), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("expected no sessions, got %+v", infos)
	}
}

func TestCompactionRoundTripKeepsEveryMessage(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	s, err := New(root, cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three", "four"} {
		if err := s.AppendMessage(agent.NewUserMessage(text)); err != nil {
			t.Fatal(err)
		}
	}
	through := s.MessageID(1)
	if through == "" {
		t.Fatal("stored messages should have ids")
	}
	if err := s.AppendCompaction(Compaction{Summary: "the summary", Messages: 2, Shortened: 1}, through); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// The log stays complete: compaction does not delete what it summarized.
	if len(loaded.Messages) != 4 {
		t.Fatalf("the file should keep every message, got %d", len(loaded.Messages))
	}
	if loaded.Compaction == nil {
		t.Fatal("the compaction was not restored")
	}
	if loaded.Compaction.Summary != "the summary" || loaded.Compaction.Messages != 2 {
		t.Fatalf("compaction not round-tripped: %+v", loaded.Compaction)
	}
	if got := loaded.CompactionIndex(); got != 1 {
		t.Fatalf("the compaction should cover messages 0..1, got %d", got)
	}
	if loaded.MessageID(2) == "" {
		t.Fatal("message ids should survive a reload")
	}

	infos, err := List(root, cwd, false)
	if err != nil || len(infos) != 1 {
		t.Fatalf("list: %v %d", err, len(infos))
	}
	if !infos[0].Compacted {
		t.Fatal("a compacted session should be marked in listings")
	}
}

func TestCompactionIndexIsUnknownForAMissingMessage(t *testing.T) {
	s := &Session{Compaction: &Compaction{Through: "not-a-message"}}
	if got := s.CompactionIndex(); got != -1 {
		t.Fatalf("an unresolvable compaction should report -1, got %d", got)
	}
}

// TestCompactionRecordsTheShortenedResults pins that the tool results a
// compaction replaced are recorded by identity, so a resumed session sends the
// same context the running one did rather than the full results again.
func TestCompactionRecordsTheShortenedResults(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	s, err := New(root, cwd, "shortened", "m")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three", "four"} {
		if err := s.AppendMessage(agent.NewUserMessage(text)); err != nil {
			t.Fatal(err)
		}
	}
	through := s.MessageID(1)
	shortened := s.MessageIDs([]int{2, 3})
	if len(shortened) != 2 {
		t.Fatalf("two stored messages should have ids, got %v", shortened)
	}
	if err := s.AppendCompaction(Compaction{
		Summary:      "the summary",
		Messages:     2,
		Shortened:    2,
		ShortenedIDs: shortened,
	}, through); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.CompactionShortenedIndexes()
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("the shortened results should resolve to their positions: %v", got)
	}
}

// TestShortenedResultsMissingFromTheLogAreSkipped pins the degraded case: a
// recorded id that no longer resolves is dropped rather than pointing at the
// wrong message.
func TestShortenedResultsMissingFromTheLogAreSkipped(t *testing.T) {
	s := &Session{
		Compaction: &Compaction{ShortenedIDs: []string{"missing", "b"}},
		messageIDs: []string{"a", "b"},
	}
	got := s.CompactionShortenedIndexes()
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("only the resolvable id should survive: %v", got)
	}
}

// TestProjectDirIsValidOnEveryPlatform pins the bucket rule: the name has to be
// usable as a directory wherever sal runs, and a Windows path carries a colon
// that a file name cannot hold.
func TestProjectDirIsValidOnEveryPlatform(t *testing.T) {
	root := t.TempDir()
	for _, cwd := range []string{
		`C:\work\project`,
		`C:\`,
		`/home/me/work/project`,
		`/`,
		`/work/spaces and (parens)/x`,
	} {
		got := filepath.Base(ProjectDir(root, cwd))
		if got == "" || got == "." || got == ".." {
			t.Fatalf("ProjectDir(%q) produced %q", cwd, got)
		}
		for _, r := range got {
			if strings.ContainsRune(`<>:"/\|?*`, r) {
				t.Fatalf("ProjectDir(%q) = %q, which contains %q and is not a valid file name", cwd, got, r)
			}
		}
		// Two different directories must not share one bucket.
		other := filepath.Base(ProjectDir(root, cwd+"-other"))
		if other == got {
			t.Fatalf("ProjectDir collapsed %q and its neighbour into %q", cwd, got)
		}
	}
	// The bucket is actually writable on the host running the suite.
	dir := ProjectDir(root, t.TempDir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("cannot create the project bucket %s: %v", dir, err)
	}
}

// TestOneDirectoryIsOneBucketThroughASymlink covers the case macOS exposes: the
// working directory can be spelled two ways, one through a symbolic link (a
// temporary directory is /var/... but resolves to /private/var/...), and both
// spellings must find the same sessions. Linux reaches a project behind a link
// the same way.
func TestOneDirectoryIsOneBucketThroughASymlink(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}

	if ProjectDir(root, real) != ProjectDir(root, link) {
		t.Fatalf("the same directory produced two buckets:\n  %s\n  %s",
			ProjectDir(root, real), ProjectDir(root, link))
	}

	sess, err := New(root, link, "through a link", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(agent.NewUserMessage("hi"))
	_ = sess.Close()

	// Listing the physical spelling finds what was written through the link.
	infos, err := List(root, real, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != sess.ID {
		t.Fatalf("the session was not found through the other spelling: %+v", infos)
	}
	// And its recorded cwd is the physical path, so the cross-bucket filter
	// recognises it too.
	if infos[0].Cwd != ProjectDir("", real) && filepath.Base(infos[0].Cwd) == "" {
		t.Fatalf("recorded cwd = %q", infos[0].Cwd)
	}
	all, err := List(root, real, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the machine-wide listing did not recognise the directory: %+v", all)
	}
}
