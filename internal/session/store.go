package session

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/bothought/sal/internal/agent"
	"github.com/bothought/sal/internal/config"
)

// exec runs one statement that changes a conversation's metadata.
func (st *Store) exec(query string, args ...any) error {
	_, err := st.db.Exec(query, args...)
	return err
}

// decodeIDs splits the comma-joined message ids a compaction records.
func decodeIDs(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// dbName is the file inside the storage root that holds every conversation.
const dbName = "sal.db"

// DBPath is the database a storage root keeps its conversations in.
func DBPath(root string) string {
	return filepath.Join(root, dbName)
}

// RootWith returns the canonical session storage root: an explicit directory
// (--session-dir or the config file), then $SAL_SESSION_DIR, then the XDG
// data directory. The store is always opened from here.
func RootWith(dir string) string {
	if trimmed := trimSpace(dir); trimmed != "" {
		return config.ExpandPath(trimmed)
	}
	if v := trimSpace(os.Getenv("SAL_SESSION_DIR")); v != "" {
		return config.ExpandPath(v)
	}
	return config.Resolve().SessionDir
}

// Store is an open connection to a storage root.
//
// One connection is enough for a process: SQLite serialises writers itself and
// the write-ahead log lets a second connection read while the first writes. The
// pragmas set here are what make that true, so they live beside the open rather
// than in each query.
type Store struct {
	db   *sql.DB
	path string
}

// OpenStore opens (creating when needed) the database under root and brings its
// schema up to the version this program understands. On first use it also
// imports the conversations an older release left as JSONL files.
//
// Opening is the only way in: a program that only reads a listing uses OpenStore
// too, because a listing and a run must agree about the schema.
func OpenStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create the session directory: %w", err)
	}
	path := DBPath(root)
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cannot open the session database %s: %w", path, err)
	}
	// A single connection keeps the pragmas and the write ordering in one place
	// and avoids a pool of writers contending with each other.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`SELECT 1`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cannot open the session database %s: %w", path, err)
	}
	if _, err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	st := &Store{db: db, path: path}
	if err := st.importLegacy(root); err != nil {
		_ = db.Close()
		return nil, err
	}
	return st, nil
}

// Exists reports whether a database has been created under root, so a command
// that only lists can say "nothing stored" without creating one.
func Exists(root string) bool {
	_, err := os.Stat(DBPath(root))
	return err == nil
}

// Close releases the database.
func (st *Store) Close() error {
	if st == nil || st.db == nil {
		return nil
	}
	return st.db.Close()
}

// Path is the database file the store was opened from.
func (st *Store) Path() string { return st.path }

// Open returns the directory's conversation, creating it when there is none,
// and reports whether one was already there.
//
// The directory and the conversation it names are written in one transaction
// through the unique key on the directory, so two processes starting there at
// the same moment cannot both create one: the loser reads the winner's row.
func (st *Store) Open(cwd, name, model string) (*Session, bool, error) {
	dir := canonicalDir(cwd)
	if s, err := st.loadByDir(dir); err == nil {
		return s, true, nil
	} else if !errors.Is(err, errNoConversation) {
		return nil, false, err
	}

	id := newID()
	tx, err := st.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	ts := now()
	if _, err := tx.Exec(
		`INSERT INTO conversations(id, dir, name, model, created_at, updated_at, archived)
		 VALUES(?, ?, ?, ?, ?, ?, 0)`,
		id, dir, name, model, ts, ts,
	); err != nil {
		return nil, false, fmt.Errorf("cannot create a conversation: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO dirs(dir, conversation_id) VALUES(?, ?)`, dir, id); err != nil {
		// The directory already names a conversation: another process created
		// it between the read above and this insert.
		_ = tx.Rollback()
		s, lerr := st.loadByDir(dir)
		if lerr != nil {
			return nil, false, lerr
		}
		return s, true, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &Session{
		ID: id, Name: name, Cwd: dir, Model: model,
		UpdatedAt: parseTime(ts), store: st,
	}, false, nil
}

// New always creates a conversation for the directory, archiving whatever it
// named before. It is what "start over" means; callers that mean "continue"
// use Open.
func (st *Store) New(cwd, name, model string) (*Session, error) {
	if _, err := st.Archive(cwd); err != nil {
		return nil, err
	}
	s, _, err := st.Open(cwd, name, model)
	return s, err
}

// Archive makes the directory's conversation no longer current: it is marked
// archived and the directory stops naming it. Nothing is deleted, so it can
// still be opened by id. It reports the archived conversation's id, or "" when
// there was nothing to archive.
func (st *Store) Archive(cwd string) (string, error) {
	dir := canonicalDir(cwd)
	tx, err := st.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRow(`SELECT conversation_id FROM dirs WHERE dir = ?`, dir).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE conversations SET archived = 1, updated_at = ? WHERE id = ?`, now(), id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`DELETE FROM dirs WHERE dir = ?`, dir); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// errNoConversation reports that a directory (or id) names nothing stored.
var errNoConversation = errors.New("no stored conversation")

// loadByDir returns the conversation a directory currently names.
func (st *Store) loadByDir(dir string) (*Session, error) {
	var id string
	if err := st.db.QueryRow(`SELECT conversation_id FROM dirs WHERE dir = ?`, dir).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNoConversation
		}
		return nil, err
	}
	return st.load(id)
}

// Load returns a stored conversation by id. A full id and the 8-character
// prefix shown in listings are both accepted.
func (st *Store) Load(id string) (*Session, error) {
	id = trimSpace(id)
	if id == "" {
		return nil, errNoConversation
	}
	s, err := st.load(id)
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, errNoConversation) {
		return nil, err
	}
	// Fall back to a prefix, which is what a listing shows.
	var full string
	if qerr := st.db.QueryRow(
		`SELECT id FROM conversations WHERE id LIKE ? ORDER BY id LIMIT 1`, id+"%",
	).Scan(&full); qerr != nil {
		return nil, errNoConversation
	}
	return st.load(full)
}

// load reads one conversation and its messages in a single snapshot, so a
// resumed conversation cannot see a summary whose messages are missing or a
// message set that predates its own compaction.
func (st *Store) load(id string) (*Session, error) {
	tx, err := st.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	s := &Session{ID: id, store: st}
	var created, updated string
	var archived int
	err = tx.QueryRow(
		`SELECT dir, name, model, created_at, updated_at, archived
		 FROM conversations WHERE id = ?`, id,
	).Scan(&s.Cwd, &s.Name, &s.Model, &created, &updated, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoConversation
	}
	if err != nil {
		return nil, err
	}
	s.UpdatedAt = parseTime(updated)
	s.Archived = archived != 0
	if s.Name == "" {
		s.Name = id[:min(8, len(id))]
	}

	rows, err := tx.Query(
		`SELECT id, payload FROM messages WHERE conversation_id = ? ORDER BY seq`, id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var msgID, payload string
		if err := rows.Scan(&msgID, &payload); err != nil {
			return nil, err
		}
		var m agent.Message
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			// A message this build cannot read is skipped rather than failing
			// the whole conversation.
			continue
		}
		s.Messages = append(s.Messages, m)
		s.messageIDs = append(s.messageIDs, msgID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var c Compaction
	var shortenedIDs string
	err = tx.QueryRow(
		`SELECT summary, through_id, messages, shortened, shortened_ids
		 FROM compactions WHERE conversation_id = ?`, id,
	).Scan(&c.Summary, &c.Through, &c.Messages, &c.Shortened, &shortenedIDs)
	if err == nil {
		c.ShortenedIDs = decodeIDs(shortenedIDs)
		s.Compaction = &c
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return s, nil
}

// List returns the current conversation of every directory, newest first, plus
// the archived conversations so a listing can offer them by id.
func (st *Store) List() ([]Info, error) {
	rows, err := st.db.Query(
		`SELECT c.id, c.name, c.dir, c.model, c.updated_at, c.archived,
		        (SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.id),
		        EXISTS(SELECT 1 FROM compactions k WHERE k.conversation_id = c.id),
		        EXISTS(SELECT 1 FROM dirs d WHERE d.conversation_id = c.id)
		 FROM conversations c`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var infos []Info
	for rows.Next() {
		var info Info
		var updated string
		var archived, compacted, current int
		if err := rows.Scan(&info.ID, &info.Name, &info.Cwd, &info.Model, &updated,
			&archived, &info.Messages, &compacted, &current); err != nil {
			return nil, err
		}
		info.UpdatedAt = parseTime(updated)
		info.Archived = archived != 0
		info.Compacted = compacted != 0
		info.Current = current != 0
		infos = append(infos, info)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortInfos(infos)
	return infos, nil
}
