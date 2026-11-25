# Session store

The technical design of the storage half of
`design/features/basic-functions` §5. The behaviour a user sees is described
there; this document says how it is built.

## 1. What the store owns

The store owns every byte sal persists about a conversation: the conversation,
its messages, its compaction, its name and model, and whether it is archived.
Nothing else writes storage, and no front end learns the layout.

It answers four questions:

- **Which conversation belongs to this directory?** At most one; starting sal in
  a directory continues it.
- **Open or create.** Getting a conversation for a directory returns what is
  there or creates one, and says which happened.
- **Archive and start over.** The current conversation stops being the
  directory's; it is kept and marked archived, and a fresh one takes its place.
- **List and load.** Every stored conversation for `--list-sessions`, and lookup
  by id for `--session`.

## 2. Why a database

A directory's conversation used to be a file, and the program had to provide
exclusive creation, waiting for concurrent writers, tolerating half-written
lines, and choosing the current file. A transactional store replaces those rules
with schema constraints:

- uniqueness is declared, not enforced by convention;
- a write is atomic, so a crash leaves the previous state rather than a fragment;
- ordering is a column, not a timestamp parsed out of text;
- the schema has a version, and upgrading is a migration.

The engine is embedded and pure Go, so the binary stays static and cross-compiles
without a C toolchain. Write-ahead logging and a busy timeout let a second
process read while the first writes.

## 3. Schema

Five tables:

- **Meta.** Bookkeeping the schema cannot express, such as whether the one-time
  legacy import ran.
- **Dirs.** One row per canonical working directory, naming the conversation it
  currently continues. A unique index on the directory is the invariant “one
  directory, one conversation”.
- **Conversations.** One row per conversation ever created: id, directory, name,
  model, creation and update time, and whether it is archived.
- **Messages.** The conversation's messages in append order. The payload is the
  neutral message the agent uses, opaque to the store.
- **Compactions.** The summary that replaced a prefix, the identity of the last
  message it covers, and the identities of tool results it shortened. At most one
  per conversation; a later compaction replaces it.

Compactions refer to messages by identity, not position, so they survive a
reload and any change in how messages are counted.

## 4. Versioning and migrations

The database records its schema version. On startup the store reads it and
applies the migrations it knows, in order, one transaction each, until it reaches
the version the program understands.

- **A newer database is refused.** A version above the program's own means a
  newer sal wrote it; the store returns an error naming both versions.
- **Each migration is atomic.** An interrupted start leaves a database the next
  start can bring forward.
- **Migrations are additive.** A release adds a migration; it never edits the
  meaning of an existing one or rewrites conversation content.

The import of the previous JSONL layout runs once, in a transaction, records
that it ran, and imports only the newest file of each directory. The files are
never modified.

## 5. Invariants

- **One directory, one current conversation.** Opening, archiving and starting
  over all go through the unique directory mapping.
- **The store is the only writer.** No front end and not the agent touches the
  database; they append through the conversation handle, which owns the
  transaction.
- **Appends are ordered and atomic.** A message is committed on its own, in the
  order it entered the conversation; a crash loses at most the message being
  written.
- **Archiving never deletes.** The directory pointer is cleared and the
  conversation marked; both rows remain, and `--session` can still open it.
- **Reading a conversation is one snapshot.** Messages and compaction are read
  together, so a resumed conversation cannot see a summary whose messages are
  missing or a message set older than its compaction.
- **The schema version only increases**, only through documented migrations.
