package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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

func TestSessionsAreGroupedPerDirectory(t *testing.T) {
	root := t.TempDir()
	a := t.TempDir()

	sess, err := New(root, a, "in-a", "m")
	if err != nil {
		t.Fatal(err)
	}
	// Close the file: Windows refuses to remove a directory whose file is still
	// open, so an unclosed handle fails the test's own cleanup.
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	all, _ := List(root)
	if len(all) != 1 || all[0].Cwd != canonicalDir(a) {
		t.Fatalf("expected exactly one conversation, under dir a, got %+v", all)
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

	infos, err := List(root)
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

	// Listing finds what was written through the link.
	infos, err := List(root)
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
	all, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the listing did not recognise the directory: %+v", all)
	}
}

// TestOneDirectoryOneFiles covers the model: a directory has one conversation at
// a fixed path, reopened rather than duplicated.
func TestOneDirectoryOneFile(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	first, resumed, err := Open(root, cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Fatal("the first open must create")
	}
	_ = first.AppendMessage(agent.NewUserMessage("remember me"))
	firstID := first.ID
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if want := Path(root, cwd); first.Path != want {
		t.Fatalf("session path = %s, want %s", first.Path, want)
	}

	second, resumed, err := Open(root, cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed {
		t.Fatal("a second open must resume")
	}
	if second.ID != firstID {
		t.Fatalf("a second open created a new session (%s -> %s)", firstID, second.ID)
	}
	if len(second.Messages) != 1 || second.Messages[0].Content != "remember me" {
		t.Fatalf("the conversation was not restored: %+v", second.Messages)
	}
	_ = second.Close()

	// Reopening does not add a second file.
	entries, err := os.ReadDir(filepath.Dir(second.Path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one file in the bucket, got %d: %v", len(entries), entries)
	}
}

// TestArchiveKeepsThePreviousConversation covers --new: starting over moves the
// file aside instead of deleting it.
func TestArchiveKeepsThePreviousConversation(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	s, _, err := Open(root, cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.AppendMessage(agent.NewUserMessage("the old work"))
	oldPath := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	archived, err := Archive(root, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if archived == "" {
		t.Fatal("Archive reported nothing to move")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("the conversation is still at %s", oldPath)
	}
	kept, err := Load(archived)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.Messages) != 1 || kept.Messages[0].Content != "the old work" {
		t.Fatalf("the archived conversation was not readable: %+v", kept.Messages)
	}

	// After archiving, opening starts fresh.
	fresh, resumed, err := Open(root, cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if resumed {
		t.Fatal("opening after --new must start over")
	}
	if len(fresh.Messages) != 0 {
		t.Fatalf("the new conversation carries messages: %+v", fresh.Messages)
	}
}

// TestOpenAdoptsThePreviousLayout covers a directory written by an older
// release: the newest timestamped file becomes the conversation, and the rest
// are left alone.
func TestOpenAdoptsThePreviousLayout(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	dir := ProjectDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLegacy := func(name, content string) string {
		t.Helper()
		s := &Session{ID: "legacy-" + name, Cwd: canonicalDir(cwd), Path: filepath.Join(dir, name)}
		if err := s.open(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.write(Entry{Type: "session", SessionID: s.ID, Cwd: s.Cwd}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.write(Entry{Type: "message", Message: &agent.Message{Role: agent.RoleUser, Content: content}}); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return s.Path
	}
	writeLegacy("20260101-000000-aaaaaaaa.jsonl", "older")
	newest := writeLegacy("20260102-000000-bbbbbbbb.jsonl", "newest")

	s, resumed, err := Open(root, cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !resumed {
		t.Fatal("an adopted conversation counts as resumed")
	}
	if len(s.Messages) != 1 || s.Messages[0].Content != "newest" {
		t.Fatalf("did not adopt the newest file: %+v", s.Messages)
	}
	if _, err := os.Stat(newest); !os.IsNotExist(err) {
		t.Fatalf("the adopted file should have moved to %s", Path(root, cwd))
	}
	// The older one is untouched.
	if _, err := os.Stat(filepath.Join(dir, "20260101-000000-aaaaaaaa.jsonl")); err != nil {
		t.Fatalf("the older conversation was disturbed: %v", err)
	}
}

// TestEmptyFileIsNotAConversation covers a session file whose creation was
// interrupted before the header was written: the leftover must not be resumed
// as a conversation with no id and no directory.
func TestEmptyFileIsNotAConversation(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	path := Path(root, cwd)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	s, resumed, err := Open(root, cwd, "wanted", "m")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if resumed {
		t.Fatal("a file with no header is a leftover, not a conversation")
	}
	if s.ID == "" || s.ID == "session" {
		t.Fatalf("the new conversation has no id of its own: %q", s.ID)
	}
	if s.Cwd == "" {
		t.Fatal("the new conversation did not record its directory")
	}
	if s.Name != "wanted" {
		t.Fatalf("the new conversation lost its name: %q", s.Name)
	}
}

// TestConcurrentCreatesLeaveOneConversation covers two sal processes starting
// in the same directory at once: one creates the conversation, the other opens
// it, and the file holds a single header either way.
func TestConcurrentCreatesLeaveOneConversation(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	const starters = 4
	sessions := make([]*Session, starters)
	errs := make([]error, starters)
	var wg sync.WaitGroup
	for i := 0; i < starters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sessions[i], _, errs[i] = Open(root, cwd, "rival", "m")
		}(i)
	}
	wg.Wait()

	ids := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("starter %d failed: %v", i, err)
		}
		ids[sessions[i].ID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("the starters disagree about the conversation: %v", ids)
	}
	for _, s := range sessions {
		_ = s.Close()
	}

	data, err := os.ReadFile(Path(root, cwd))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), `"type":"session"`); n != 1 {
		t.Fatalf("the file holds %d session headers, want 1:\n%s", n, data)
	}
}

// TestArchiveIsNotListed covers --new: the conversation moved aside stays on
// disk but is no longer the directory's entry in the listing.
func TestArchiveIsNotListed(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	old, _, err := Open(root, cwd, "the old one", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = old.AppendMessage(agent.NewUserMessage("old work"))
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	archived, err := Archive(root, cwd)
	if err != nil {
		t.Fatal(err)
	}

	// Without a new conversation yet, the directory has none to show.
	if infos, err := List(root); err != nil {
		t.Fatal(err)
	} else if len(infos) != 0 {
		t.Fatalf("an archived conversation was listed: %+v", infos)
	}

	fresh, err := New(root, cwd, "the new one", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = fresh.AppendMessage(agent.NewUserMessage("new work"))
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	infos, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected one conversation per directory, got %+v", infos)
	}
	if infos[0].Path != Path(root, cwd) {
		t.Fatalf("listed %s, want the current conversation %s (archived: %s)", infos[0].Path, Path(root, cwd), archived)
	}
	if infos[0].Name != "the new one" {
		t.Fatalf("listed the wrong conversation: %+v", infos[0])
	}
}
