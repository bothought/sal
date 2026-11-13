package session

import (
	"database/sql"
	"fmt"
)

// schemaVersion is the schema this program was built against. It only ever
// increases, and a database above it is refused rather than opened.
const schemaVersion = 1

// migration is one step from one schema version to the next. Each runs in its
// own transaction, so an interrupted start leaves a version the next start can
// bring forward.
type migration struct {
	version int
	apply   func(tx *sql.Tx) error
}

// migrations are applied in order until the database reaches schemaVersion.
// A released migration is never edited: its meaning is part of the on-disk
// history, and changing it would rewrite databases that already ran it.
var migrations = []migration{
	{version: 1, apply: createSchema},
}

// createSchema lays out the store: a directory names at most one current
// conversation, a conversation owns ordered messages and at most one
// compaction, and archiving is a flag rather than a deletion.
func createSchema(tx *sql.Tx) error {
	stmts := []string{
		// Bookkeeping the schema cannot express: which one-time steps ran.
		`CREATE TABLE meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		// One row per canonical working directory, naming the conversation it
		// continues. The primary key is the invariant "one directory, one
		// conversation", enforced where it cannot be bypassed.
		`CREATE TABLE dirs (
			dir            TEXT PRIMARY KEY,
			conversation_id TEXT NOT NULL
		)`,
		// One row per conversation ever created. An archived conversation is
		// kept here but named by no directory.
		`CREATE TABLE conversations (
			id         TEXT PRIMARY KEY,
			dir        TEXT NOT NULL,
			name       TEXT NOT NULL DEFAULT '',
			model      TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			archived   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX conversations_dir ON conversations(dir, archived)`,
		// Messages in append order. The payload is the neutral message the agent
		// already uses, stored opaque to this package.
		`CREATE TABLE messages (
			id              TEXT PRIMARY KEY,
			conversation_id TEXT NOT NULL REFERENCES conversations(id),
			seq             INTEGER NOT NULL,
			payload         TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX messages_order ON messages(conversation_id, seq)`,
		// At most one compaction per conversation: the summary that replaced a
		// prefix, the last message it covers, and the tool results it shortened,
		// all by message identity rather than position.
		`CREATE TABLE compactions (
			conversation_id TEXT PRIMARY KEY REFERENCES conversations(id),
			summary         TEXT NOT NULL,
			through_id      TEXT NOT NULL,
			messages        INTEGER NOT NULL,
			shortened       INTEGER NOT NULL DEFAULT 0,
			shortened_ids   TEXT NOT NULL DEFAULT ''
		)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("creating the session schema: %w", err)
		}
	}
	return nil
}

// migrate brings db to schemaVersion. It reports the version it found, so the
// caller can tell a fresh database from an upgraded one.
func migrate(db *sql.DB) (from int, err error) {
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&from); err != nil {
		return 0, fmt.Errorf("reading the session schema version: %w", err)
	}
	if from > schemaVersion {
		return from, fmt.Errorf("the session database is at schema version %d, but this sal understands %d; use a newer release or point --session-dir elsewhere", from, schemaVersion)
	}
	for _, m := range migrations {
		if m.version <= from {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return from, err
		}
	}
	return from, nil
}

// applyMigration runs one migration and records the new version atomically, so
// a database never claims a version whose changes are missing.
func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("starting schema migration %d: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.apply(tx); err != nil {
		return fmt.Errorf("schema migration %d: %w", m.version, err)
	}
	// PRAGMA does not take a bind parameter, and the value is a constant of this
	// program rather than user input.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("recording schema version %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing schema migration %d: %w", m.version, err)
	}
	return nil
}
