package session

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bothought/sal/internal/agent"
)

// legacyKey records that the one-time import of the previous layout ran, so a
// later start never repeats it.
const legacyKey = "legacy_jsonl_import"

// legacyEntry is one line of a file the previous layout wrote. The store reads
// it only to import; nothing writes this format any more.
type legacyEntry struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	Timestamp string         `json:"timestamp"`
	SessionID string         `json:"sessionId"`
	Cwd       string         `json:"cwd"`
	Model     string         `json:"model"`
	Name      string         `json:"name"`
	Message   *agent.Message `json:"message"`
	Compacted *legacyComp    `json:"compacted"`
}

// legacyComp is a compaction as the previous layout recorded it.
type legacyComp struct {
	Summary      string   `json:"summary"`
	Through      string   `json:"through"`
	Messages     int      `json:"messages"`
	Shortened    int      `json:"shortened"`
	ShortenedIDs []string `json:"shortenedIds"`
}

// importLegacy brings the conversations an older release stored as JSONL files
// into the database, once.
//
// The newest file of each directory's bucket becomes that directory's
// conversation; older files in the same bucket are left alone, as the previous
// layout's own rule said. The files are read, never modified or deleted. The
// whole import is one transaction whose final step records that it ran, so an
// interrupted import leaves the database untouched and the next start repeats
// it from the beginning.
func (st *Store) importLegacy(root string) error {
	var done string
	err := st.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, legacyKey).Scan(&done)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	found, err := findLegacyRoots(root)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		// Nothing to import. The marker is deliberately not written, so a store
		// created before an old file appears still imports it; the import is
		// idempotent, so the rescan is safe.
		return nil
	}

	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	imported := 0
	for _, dir := range found {
		n, err := importLegacyDir(tx, dir)
		if err != nil {
			return err
		}
		imported += n
	}
	if imported == 0 {
		return nil
	}
	if _, err := tx.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		legacyKey, now(),
	); err != nil {
		return err
	}
	return tx.Commit()
}

// findLegacyRoots returns the buckets that hold JSONL files from the previous
// layout, skipping the database itself and anything already archived.
func findLegacyRoots(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if hasLegacyFile(dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

// hasLegacyFile reports whether a bucket holds at least one readable JSONL
// session file.
func hasLegacyFile(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// importLegacyDir imports the newest readable file in one bucket, reporting how
// many conversations it imported. It is idempotent: a bucket that already names
// a conversation is left alone, so running it again adds nothing.
func importLegacyDir(tx *sql.Tx, dir string) (int, error) {
	newest, err := newestLegacyFile(dir)
	if err != nil || newest == "" {
		return 0, err
	}
	conv, err := readLegacyFile(newest)
	if err != nil {
		// A file this build cannot read is skipped, not fatal: the rest of the
		// store still comes up.
		return 0, nil
	}
	if conv.ID == "" || conv.Cwd == "" {
		return 0, nil
	}
	// A directory that already names a conversation keeps it; the import only
	// fills in what the store does not have.
	var have string
	err = tx.QueryRow(`SELECT conversation_id FROM dirs WHERE dir = ?`, conv.Cwd).Scan(&have)
	if err == nil {
		return 0, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	// The column carries the same timestamp format everything else writes, so a
	// reader parses it the same way.
	stamp := conv.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if conv.UpdatedAt.IsZero() {
		stamp = now()
	}
	if _, err := tx.Exec(
		`INSERT INTO conversations(id, dir, name, model, created_at, updated_at, archived)
		 VALUES(?, ?, ?, ?, ?, ?, 0)
		 ON CONFLICT(id) DO NOTHING`,
		conv.ID, conv.Cwd, conv.Name, conv.Model, stamp, stamp,
	); err != nil {
		return 0, fmt.Errorf("importing %s: %w", newest, err)
	}
	if _, err := tx.Exec(`INSERT INTO dirs(dir, conversation_id) VALUES(?, ?)`, conv.Cwd, conv.ID); err != nil {
		return 0, fmt.Errorf("importing %s: %w", newest, err)
	}
	for i, m := range conv.Messages {
		payload, err := json.Marshal(m)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(
			`INSERT INTO messages(id, conversation_id, seq, payload) VALUES(?, ?, ?, ?)
			 ON CONFLICT(conversation_id, seq) DO NOTHING`,
			conv.messageIDs[i], conv.ID, i, string(payload),
		); err != nil {
			return 0, fmt.Errorf("importing %s: %w", newest, err)
		}
	}
	if c := conv.Compaction; c != nil {
		if _, err := tx.Exec(
			`INSERT INTO compactions(conversation_id, summary, through_id, messages, shortened, shortened_ids)
			 VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(conversation_id) DO NOTHING`,
			conv.ID, c.Summary, c.Through, c.Messages, c.Shortened, strings.Join(c.ShortenedIDs, ","),
		); err != nil {
			return 0, fmt.Errorf("importing %s: %w", newest, err)
		}
	}
	return 1, nil
}

// newestLegacyFile is the most recently written JSONL file in a bucket.
func newestLegacyFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var newest string
	var newestAt time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, serr := os.Stat(p)
		if serr != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = p, info.ModTime()
		}
	}
	return newest, nil
}

// readLegacyFile reconstructs a conversation from one JSONL file of the
// previous layout.
func readLegacyFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &Session{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e legacyEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // a truncated final line is tolerated, as before
		}
		switch e.Type {
		case "session":
			s.ID = e.SessionID
			s.Cwd = e.Cwd
			s.Model = e.Model
			s.Name = e.Name
		case "message":
			if e.Message != nil {
				s.Messages = append(s.Messages, *e.Message)
				id := e.ID
				if id == "" {
					id = newID()
				}
				s.messageIDs = append(s.messageIDs, id)
			}
		case "name":
			s.Name = e.Name
		case "model_change":
			s.Model = e.Model
		case "compaction":
			if e.Compacted != nil {
				s.Compaction = &Compaction{
					Summary:      e.Compacted.Summary,
					Through:      e.Compacted.Through,
					Messages:     e.Compacted.Messages,
					Shortened:    e.Compacted.Shortened,
					ShortenedIDs: e.Compacted.ShortenedIDs,
				}
			}
		}
		if e.Timestamp != "" {
			if ts, perr := time.Parse(time.RFC3339Nano, e.Timestamp); perr == nil && ts.After(s.UpdatedAt) {
				s.UpdatedAt = ts
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if s.ID == "" {
		s.ID = newID()
	}
	if s.Cwd == "" {
		// Without a recorded directory the file cannot be tied to one; the
		// caller skips it.
		return s, nil
	}
	s.Cwd = canonicalDir(s.Cwd)
	if s.UpdatedAt.IsZero() {
		// A file with no usable entry timestamp still has a modification time,
		// which is where the previous layout's own scratch files were dated.
		if fi, err := os.Stat(path); err == nil {
			s.UpdatedAt = fi.ModTime().UTC()
		}
	}
	return s, nil
}
