// Package session stores conversations in a single embedded SQLite database.
//
// A working directory owns exactly one current conversation; opening sal there
// continues it. Archiving keeps a conversation without it being current, and no
// code path deletes one. The schema is versioned and migrated on open, and an
// older release's JSONL files are imported once, without being modified.
package session

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bothought/sal/internal/agent"
)

// canonicalDir resolves a working directory to the path that identifies it, so
// the same directory always names the same conversation.
//
// An absolute path is not enough: a path can be reached through a symbolic link
// (macOS reaches temporary directories through /private/var, and a person may
// keep a project behind a link), and the two spellings would otherwise become
// two conversations. Resolving first makes the identity physical. A path that
// cannot be resolved is used as written.
func canonicalDir(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// Compaction is a recorded compaction: the summary that replaced a prefix of
// the conversation, and which messages it covers. The messages themselves stay
// stored, so the conversation remains a complete record and only the active
// context is smaller.
type Compaction struct {
	Summary string
	// Through is the id of the last message the summary replaces. It is an id
	// rather than an index so it survives any change in how messages are
	// counted.
	Through  string
	Messages int
	// Shortened is how many tool results in the kept tail were replaced with a
	// marker, and ShortenedIDs names the messages they were, so a resumed
	// conversation sends the same context the running one did.
	Shortened    int
	ShortenedIDs []string
}

// Info summarises a stored conversation.
type Info struct {
	ID        string
	Name      string
	Cwd       string
	Model     string
	Messages  int
	UpdatedAt time.Time
	// Compacted reports that the conversation has a recorded compaction, so a
	// listing can show why it is shorter than its history.
	Compacted bool
	// Archived reports that the conversation is no longer what its directory
	// continues.
	Archived bool
	// Current reports that a directory names this conversation now.
	Current bool
}

// Session is one stored conversation. Messages are kept in memory for the agent
// and appended to the store as they enter the conversation.
type Session struct {
	ID       string
	Name     string
	Cwd      string
	Model    string
	Messages []agent.Message
	// Compaction is the recorded compaction, or nil when the context is the
	// whole conversation.
	Compaction *Compaction
	// UpdatedAt is when the newest change was stored, used to order listings.
	UpdatedAt time.Time
	// Archived reports that the conversation is no longer current for its
	// directory.
	Archived bool

	store      *Store
	mu         sync.Mutex
	messageIDs []string
}

// Close releases this handle. The store holds the database, so a conversation
// has nothing of its own to flush: every write was committed when it happened.
func (s *Session) Close() error { return nil }

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(v string) time.Time {
	ts, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return ts
}

func trimSpace(s string) string { return strings.TrimSpace(s) }

// AppendMessage stores a conversation message and keeps the in-memory history.
func (s *Session) AppendMessage(m agent.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		// Ephemeral modes (-p/--no-session) keep the conversation in memory.
		s.Messages = append(s.Messages, m)
		s.messageIDs = append(s.messageIDs, "")
		return nil
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	id := newID()
	tx, err := s.store.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// The position is computed by the statement that writes it, so two
	// processes appending at the same moment cannot claim the same one.
	if _, err := tx.Exec(
		`INSERT INTO messages(id, conversation_id, seq, payload)
		 SELECT ?, ?, COALESCE(MAX(seq) + 1, 0), ? FROM messages WHERE conversation_id = ?`,
		id, s.ID, string(payload), s.ID,
	); err != nil {
		return fmt.Errorf("cannot store a message: %w", err)
	}
	if err := touch(tx, s.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.Messages = append(s.Messages, m)
	s.messageIDs = append(s.messageIDs, id)
	s.UpdatedAt = time.Now().UTC()
	return nil
}

// MessageID returns the stored id of the message at index i, or "" when it is
// unknown (an ephemeral conversation, or an index outside the conversation). It
// is what a compaction records so the summary can be tied back to the messages
// it covers.
func (s *Session) MessageID(i int) string {
	if i < 0 || i >= len(s.messageIDs) {
		return ""
	}
	return s.messageIDs[i]
}

// MessageIDs is MessageID for a list of indexes, skipping the ones it cannot
// resolve, so a compaction can record a set of messages by identity.
func (s *Session) MessageIDs(indexes []int) []string {
	if len(indexes) == 0 {
		return nil
	}
	out := make([]string, 0, len(indexes))
	for _, i := range indexes {
		if id := s.MessageID(i); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// CompactionIndex is the index in Messages of the last message a recorded
// compaction covers, or -1 when there is no compaction or it cannot be located.
func (s *Session) CompactionIndex() int {
	if s.Compaction == nil || s.Compaction.Through == "" {
		return -1
	}
	return s.indexByID(s.Compaction.Through)
}

// CompactionShortenedIndexes maps the recorded shortened tool results back to
// their positions in Messages, skipping any whose id is no longer present.
func (s *Session) CompactionShortenedIndexes() []int {
	if s.Compaction == nil || len(s.Compaction.ShortenedIDs) == 0 {
		return nil
	}
	out := make([]int, 0, len(s.Compaction.ShortenedIDs))
	for _, id := range s.Compaction.ShortenedIDs {
		if i := s.indexByID(id); i >= 0 {
			out = append(out, i)
		}
	}
	return out
}

// indexByID is the position in Messages of the message with the given stored
// id, or -1.
func (s *Session) indexByID(id string) int {
	for i, got := range s.messageIDs {
		if got == id {
			return i
		}
	}
	return -1
}

// SetName records a new display name.
func (s *Session) SetName(name string) error {
	s.Name = name
	if s.store == nil {
		return nil
	}
	return s.store.exec(
		`UPDATE conversations SET name = ?, updated_at = ? WHERE id = ?`, name, now(), s.ID,
	)
}

// AppendCompaction records that a summary replaced a prefix of the conversation.
// The messages it covers stay stored; this row is what makes the active context
// smaller on resume. throughID is the id of the last message it replaces.
func (s *Session) AppendCompaction(c Compaction, throughID string) error {
	if c.Through == "" {
		c.Through = throughID
	}
	s.Compaction = &c
	if s.store == nil {
		return nil
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO compactions(conversation_id, summary, through_id, messages, shortened, shortened_ids)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(conversation_id) DO UPDATE SET
		   summary = excluded.summary,
		   through_id = excluded.through_id,
		   messages = excluded.messages,
		   shortened = excluded.shortened,
		   shortened_ids = excluded.shortened_ids`,
		s.ID, c.Summary, c.Through, c.Messages, c.Shortened, strings.Join(c.ShortenedIDs, ","),
	); err != nil {
		return fmt.Errorf("cannot store the compaction: %w", err)
	}
	if err := touch(tx, s.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetModel records a model switch.
func (s *Session) SetModel(model string) error {
	s.Model = model
	if s.store == nil {
		return nil
	}
	return s.store.exec(
		`UPDATE conversations SET model = ?, updated_at = ? WHERE id = ?`, model, now(), s.ID,
	)
}

// touch records that the conversation changed, in the caller's transaction.
func touch(tx *sql.Tx, id string) error {
	_, err := tx.Exec(`UPDATE conversations SET updated_at = ? WHERE id = ?`, now(), id)
	return err
}

// sortInfos orders conversations newest first, with a deterministic tie-break
// when two timestamps collide.
func sortInfos(infos []Info) {
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].UpdatedAt.Equal(infos[j].UpdatedAt) {
			return infos[i].UpdatedAt.After(infos[j].UpdatedAt)
		}
		return infos[i].ID < infos[j].ID
	})
}
