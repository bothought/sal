package session

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bothought/sal/internal/agent"
)

// openStore opens a store in a fresh root, closing it with the test.
func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	st, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, root
}

// writeLegacyFile writes one JSONL file in the previous layout's format.
func writeLegacyFile(t *testing.T, path string, entries ...legacyEntry) {
	t.Helper()
	var lines []string
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	st, _ := openStore(t)
	cwd := t.TempDir()

	s, resumed, err := st.Open(cwd, "my task", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Fatal("a fresh directory must not resume")
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

	again, resumed, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed {
		t.Fatal("a second open must resume")
	}
	if again.ID != s.ID {
		t.Fatalf("id mismatch: %s vs %s", again.ID, s.ID)
	}
	if again.Name != "renamed" {
		t.Fatalf("name not persisted: %q", again.Name)
	}
	if again.Model != "other-model" {
		t.Fatalf("model not persisted: %q", again.Model)
	}
	if len(again.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(again.Messages))
	}
	if again.Messages[0].Content != "hello" || again.Messages[1].Content != "hi there" {
		t.Fatalf("messages not reconstructed: %+v", again.Messages)
	}
	if again.Cwd != canonicalDir(cwd) {
		t.Fatalf("directory not recorded: %q", again.Cwd)
	}
}

// TestMessagesKeepTheirOrder pins that the order is stored, not re-derived from
// the clock: a conversation comes back the way it was appended.
func TestMessagesKeepTheirOrder(t *testing.T) {
	st, _ := openStore(t)
	cwd := t.TempDir()
	s, _, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"a", "b", "c", "d"} {
		m := agent.NewUserMessage(text)
		// A clock that does not move is the hostile case for ordering by time.
		m.Timestamp = "2026-01-01T00:00:00Z"
		if err := s.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	again, _, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range again.Messages {
		got = append(got, m.Content)
	}
	if strings.Join(got, "") != "abcd" {
		t.Fatalf("messages came back out of order: %v", got)
	}
}

func TestSessionsAreGroupedPerDirectory(t *testing.T) {
	st, _ := openStore(t)
	a := t.TempDir()

	if _, _, err := st.Open(a, "in-a", "m"); err != nil {
		t.Fatal(err)
	}
	all, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Cwd != canonicalDir(a) {
		t.Fatalf("expected exactly one conversation, under dir a, got %+v", all)
	}
	if !all[0].Current {
		t.Fatalf("a directory's conversation should be current: %+v", all[0])
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
	st, _ := openStore(t)
	cwd := t.TempDir()

	s, _, err := st.Open(cwd, "", "m")
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

	loaded, _, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	// The history stays complete: compaction does not delete what it summarized.
	if len(loaded.Messages) != 4 {
		t.Fatalf("the store should keep every message, got %d", len(loaded.Messages))
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

	infos, err := st.List()
	if err != nil || len(infos) != 1 {
		t.Fatalf("list: %v %d", err, len(infos))
	}
	if !infos[0].Compacted {
		t.Fatal("a compacted conversation should be marked in listings")
	}
}

// TestCompactionReplacesThePreviousOne pins that a second compaction updates the
// single row rather than stacking summaries.
func TestCompactionReplacesThePreviousOne(t *testing.T) {
	st, _ := openStore(t)
	cwd := t.TempDir()
	s, _, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two", "three", "four"} {
		if err := s.AppendMessage(agent.NewUserMessage(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendCompaction(Compaction{Summary: "first", Messages: 1}, s.MessageID(0)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendCompaction(Compaction{Summary: "second", Messages: 3}, s.MessageID(2)); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Compaction == nil || loaded.Compaction.Summary != "second" {
		t.Fatalf("the latest compaction should win: %+v", loaded.Compaction)
	}
	if got := loaded.CompactionIndex(); got != 2 {
		t.Fatalf("the compaction should cover messages 0..2, got %d", got)
	}
}

func TestCompactionIndexIsUnknownForAMissingMessage(t *testing.T) {
	s := &Session{Compaction: &Compaction{Through: "not-a-message"}}
	if got := s.CompactionIndex(); got != -1 {
		t.Fatalf("an unresolvable compaction should report -1, got %d", got)
	}
}

// TestCompactionRecordsTheShortenedResults pins that the tool results a
// compaction replaced are recorded by identity, so a resumed conversation sends
// the same context the running one did rather than the full results again.
func TestCompactionRecordsTheShortenedResults(t *testing.T) {
	st, _ := openStore(t)
	cwd := t.TempDir()
	s, _, err := st.Open(cwd, "shortened", "m")
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

	loaded, _, err := st.Open(cwd, "", "m")
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

// TestOneDirectoryIsOneConversationThroughASymlink pins that the identity of a
// directory is physical: two spellings of the same directory continue the same
// conversation.
func TestOneDirectoryIsOneConversationThroughASymlink(t *testing.T) {
	st, _ := openStore(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	first, _, err := st.Open(real, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	second, resumed, err := st.Open(link, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed || second.ID != first.ID {
		t.Fatalf("the same directory through a link became another conversation: %s vs %s", first.ID, second.ID)
	}
	if all, err := st.List(); err != nil || len(all) != 1 {
		t.Fatalf("expected one conversation, got %+v (%v)", all, err)
	}
}

// TestArchiveKeepsThePreviousConversation covers --new: starting over archives
// the conversation instead of deleting it.
func TestArchiveKeepsThePreviousConversation(t *testing.T) {
	st, _ := openStore(t)
	cwd := t.TempDir()

	old, _, err := st.Open(cwd, "the old one", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.AppendMessage(agent.NewUserMessage("the old work")); err != nil {
		t.Fatal(err)
	}
	archivedID, err := st.Archive(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if archivedID != old.ID {
		t.Fatalf("archived %q, want %q", archivedID, old.ID)
	}

	// After archiving, opening starts fresh.
	fresh, resumed, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Fatal("opening after --new must start over")
	}
	if len(fresh.Messages) != 0 {
		t.Fatalf("the new conversation carries messages: %+v", fresh.Messages)
	}
	if fresh.ID == old.ID {
		t.Fatal("the new conversation reused the archived id")
	}

	// The archived conversation is kept, readable by id, and marked.
	kept, err := st.Load(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.Messages) != 1 || kept.Messages[0].Content != "the old work" {
		t.Fatalf("the archived conversation was not readable: %+v", kept.Messages)
	}
	if !kept.Archived {
		t.Fatal("the archived conversation is not marked as archived")
	}
	infos, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	var sawCurrent, sawArchived bool
	for _, info := range infos {
		switch info.ID {
		case old.ID:
			sawArchived = info.Archived
		case fresh.ID:
			sawCurrent = info.Current && !info.Archived
		}
	}
	if !sawArchived || !sawCurrent {
		t.Fatalf("the listing should show one archived and one current: %+v", infos)
	}
}

// TestOpenReturnsTheDirectorysConversationUnderConcurrency pins the invariant
// "one directory, one conversation": simultaneous starts agree on one id.
func TestOpenReturnsTheDirectorysConversationUnderConcurrency(t *testing.T) {
	st, _ := openStore(t)
	cwd := t.TempDir()

	const starters = 4
	sessions := make([]*Session, starters)
	errs := make([]error, starters)
	var wg sync.WaitGroup
	for i := 0; i < starters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sessions[i], _, errs[i] = st.Open(cwd, "rival", "m")
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
	if all, err := st.List(); err != nil || len(all) != 1 {
		t.Fatalf("expected one stored conversation, got %+v (%v)", all, err)
	}
}

// TestNewArchivesAndStartsOver covers the store's "start over" in one step.
func TestNewArchivesAndStartsOver(t *testing.T) {
	st, _ := openStore(t)
	cwd := t.TempDir()

	first, _, err := st.Open(cwd, "first", "m")
	if err != nil {
		t.Fatal(err)
	}
	_ = first.AppendMessage(agent.NewUserMessage("old"))

	fresh, err := st.New(cwd, "second", "m")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == first.ID || len(fresh.Messages) != 0 {
		t.Fatalf("New should start a fresh conversation: %+v", fresh)
	}
	if kept, err := st.Load(first.ID); err != nil || !kept.Archived {
		t.Fatalf("the previous conversation should be archived: %+v %v", kept, err)
	}
}

// TestLoadAcceptsTheShortIdThatListingsShow pins that --session can take the
// 8-character id a person reads from --list-sessions.
func TestLoadAcceptsTheShortIdThatListingsShow(t *testing.T) {
	st, _ := openStore(t)
	cwd := t.TempDir()
	s, _, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	short := s.ID[:8]
	got, err := st.Load(short)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != s.ID {
		t.Fatalf("loaded %q for prefix %q, want %q", got.ID, short, s.ID)
	}
	if _, err := st.Load("ffffffffffffffff"); err == nil {
		t.Fatal("an unknown id should be an error")
	}
}

// TestSchemaIsVersionedAndForwardCompatible covers the migration contract: a
// fresh database is stamped with the current version, reopening is a no-op, and
// a database from a newer release is refused rather than opened.
func TestSchemaIsVersionedAndForwardCompatible(t *testing.T) {
	root := t.TempDir()
	st, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("fresh database is at version %d, want %d", version, schemaVersion)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening keeps the version and the data.
	if st, err = OpenStore(root); err != nil {
		t.Fatalf("reopening should succeed: %v", err)
	}
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("reopening changed the version to %d", version)
	}
	_ = st.Close()

	// A database written by a newer sal is refused, and not modified.
	raw, err := sql.Open("sqlite", "file:"+DBPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	if _, err := OpenStore(root); err == nil {
		t.Fatal("a database from a newer release should be refused")
	} else if !strings.Contains(err.Error(), "99") {
		t.Fatalf("the error should name the version it found: %v", err)
	}
}

// TestMessagesAreCommittedWithTheirConversation pins that a stored message
// survives a second process opening the same database, which is what makes a
// finished turn durable.
func TestMessagesAreCommittedWithTheirConversation(t *testing.T) {
	st, root := openStore(t)
	cwd := t.TempDir()
	s, _, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(agent.NewUserMessage("durable")); err != nil {
		t.Fatal(err)
	}

	// A second connection, as another sal process would have.
	other, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	got, resumed, err := other.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed {
		t.Fatal("the second process should continue the conversation")
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "durable" {
		t.Fatalf("the committed message is missing: %+v", got.Messages)
	}
}

// TestLegacyJSONLIsImportedOnce covers the move from the previous layout: the
// newest file of each bucket becomes the directory's conversation, the files are
// left alone, and a second start imports nothing again.
func TestLegacyJSONLIsImportedOnce(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	dir := filepath.Join(root, "some-bucket")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLegacy := func(name, sessionID, content string, mod time.Time) string {
		t.Helper()
		path := filepath.Join(dir, name)
		// Marshalled rather than concatenated: a Windows path in a hand-built
		// JSON string is an invalid escape, which would make the fixture (not
		// the code) the thing under test.
		writeLegacyFile(t, path, legacyEntry{
			Type: "session", ID: "h", Timestamp: "2026-01-01T00:00:00Z",
			SessionID: sessionID, Cwd: cwd, Model: "m", Name: sessionID,
		}, legacyEntry{
			Type: "message", ID: "m1", Timestamp: "2026-01-01T00:00:01Z",
			Message: &agent.Message{Role: agent.RoleUser, Content: content},
		})
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
		return path
	}
	oldPath := writeLegacy("20260101-000000-aaaaaaaa.jsonl", "aaaaaaaa00000000", "older", time.Now().Add(-time.Hour))
	newPath := writeLegacy("20260102-000000-bbbbbbbb.jsonl", "bbbbbbbb00000000", "newest", time.Now())

	st, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	infos, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("the newest file should become the one conversation, got %+v", infos)
	}
	if infos[0].ID != "bbbbbbbb00000000" {
		t.Fatalf("imported %q, want the newest file's session", infos[0].ID)
	}
	s, resumed, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed || s.ID != "bbbbbbbb00000000" {
		t.Fatalf("opening should continue the imported conversation: %+v", s)
	}
	if len(s.Messages) != 1 || s.Messages[0].Content != "newest" {
		t.Fatalf("imported messages are missing: %+v", s.Messages)
	}
	if s.UpdatedAt.IsZero() {
		t.Fatal("an imported conversation should keep a usable update time")
	}
	// The listing must not print a zero date for an imported conversation.
	if infos[0].UpdatedAt.IsZero() {
		t.Fatal("the listing shows no date for the imported conversation")
	}

	// Neither file is modified or removed.
	for _, p := range []string{oldPath, newPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the legacy file should be left alone: %v", err)
		}
	}

	// Importing is one-time: a conversation created later is not overwritten by
	// a second start.
	if _, err := st.New(cwd, "after import", "m"); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	again, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	got, resumed, err := again.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed || got.Name != "after import" {
		t.Fatalf("the second start re-imported over the current conversation: %+v", got)
	}
}

// TestLegacyFilesAppearingLaterAreStillImported pins that a store created
// before an old file exists does not mark the import as finished, so a person
// who upgrades, runs sal once on an empty root, and only then points
// --session-dir at their old files still gets them.
func TestLegacyFilesAppearingLaterAreStillImported(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	// First start: nothing stored yet.
	st, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Open(t.TempDir(), "", "m"); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	// The old files appear afterwards.
	dir := filepath.Join(root, "bucket")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLegacyFile(t, filepath.Join(dir, "20260101-000000-cccccccc.jsonl"),
		legacyEntry{Type: "session", ID: "h", Timestamp: "2026-01-01T00:00:00Z", SessionID: "cccccccc00000000", Cwd: cwd, Model: "m"},
		legacyEntry{
			Type: "message", ID: "m1", Timestamp: "2026-01-01T00:00:01Z",
			Message: &agent.Message{Role: agent.RoleUser, Content: "late"},
		})

	st, err = OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, resumed, err := st.Open(cwd, "", "m")
	if err != nil {
		t.Fatal(err)
	}
	if !resumed || s.ID != "cccccccc00000000" || len(s.Messages) != 1 {
		t.Fatalf("the late legacy file was not imported: %+v (resumed %v)", s, resumed)
	}
}

// TestListingDoesNotCreateAStore pins that reading a machine with nothing stored
// leaves it untouched, so `--list-sessions` never creates a database.
func TestListingDoesNotCreateAStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nothing-here")
	if Exists(root) {
		t.Fatal("a directory that does not exist cannot hold a store")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the root should not have been created: %v", err)
	}
}
