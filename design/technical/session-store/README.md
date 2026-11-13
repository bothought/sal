# Session store

The technical design of the storage half of
`design/features/basic-functions` §5: where conversations live, how the program
brings the store up to date, and which invariants keep the code above it honest.
The behaviour a user sees is described there; this document says how it is
built.

## 1. What the store owns

The store owns every byte sal persists about a conversation: the conversation
itself, its messages, its compaction, its name and model, and the fact that it
is archived. Nothing else in the program writes storage, and no front end learns
the layout.

It answers four questions and no more:

- **Which conversation belongs to this directory?** The answer is at most one,
  and starting sal in a directory always continues it.
- **Open or create.** Getting a conversation for a directory returns either what
  is there or a new one, and says which happened, so the program can report that
  it continued something.
- **Archive and start over.** The current conversation stops being the
  directory's; it is kept, marked as archived, and a fresh one takes its place.
- **List what is stored.** One entry per directory, for `--list-sessions`, and
  lookup by id, for `--session`.

Everything the agent loop needs — the messages and the compaction it starts
from, and a way to append as history grows — is expressed against the
conversation handle, not against the database.

## 2. Why a database, and why this one

A directory's conversation used to be a file, and the program had to provide
what a filesystem does not: exclusive creation, waiting for a concurrent writer,
tolerating a half-written last line, and deciding which of several files is
current. Those rules were the source of the last two defects in that area, and
each was a race between two processes.

A transactional store replaces all of that with two statements and a schema
constraint:

- **Uniqueness is declared, not enforced by convention.** One conversation per
  directory is a unique index on the canonical directory, so two processes
  cannot both insert; the loser gets the row the winner committed.
- **A write is atomic.** A message is inserted in its own transaction, so a
  process killed mid-write leaves either the message or nothing, never a
  fragment. There is no partial-line case to tolerate.
- **Ordering is a column**, not a timestamp parsed back out of text, so messages
  come back in the order they were appended even when the clock is not
  monotonic.
- **The schema has a version**, and upgrading is a migration, not a guess.

The store is an embedded SQLite database read through a pure-Go driver, so the
binary stays static and cross-compiles to every release target without a C
toolchain. Write-ahead logging and a busy timeout let a second sal process read
while the first writes, which is what makes `--list-sessions` in another
terminal safe.

## 3. The schema

Four tables. The names and columns belong to the code; what each carries is the
contract:

- **Dir conversations.** One row per canonical working directory: the
  conversation it currently continues. A unique index on the directory is the
  invariant "one directory, one conversation", expressed where it cannot be
  bypassed.
- **Conversations.** One row per conversation ever created: its id, its
  directory, its display name, its model, when it was created and last written,
  and whether it is archived. A directory's current conversation is the row the
  directory names; an archived one is a row no directory names any more.
- **Messages.** The conversation's messages, in append order, each with the
  neutral message payload the agent already uses. The payload is opaque to the
  store: it is serialized and returned unchanged, which is what keeps the agent
  and the storage layer from sharing a type.
- **Compactions.** The summary that replaced a prefix of a conversation, the
  identity of the last message it covers, and the identities of the tool results
  it shortened. At most one per conversation, and it replaces its predecessor
  when a conversation is compacted twice.

Message identity, rather than position, is what a compaction refers to. Positions
change when a conversation is reopened; ids do not, and the message rows carry
them.

## 4. Versioning and migration

The database records the schema version it is at. On startup the store reads
that version and applies the migrations it knows, in order, one transaction
each, until the database is at the version the program was built against.

Three rules keep this safe:

- **A newer database is refused.** A version above the program's own means a
  newer sal wrote it. The store returns an error naming both versions rather
  than opening it, because guessing at a newer schema is how data is lost.
- **Each migration is a single transaction.** A migration either completed or
  did not run, so an interrupted start leaves a database the next start can
  bring forward, not a half-migrated one.
- **Migrations are additive and documented here.** A release that changes the
  schema adds a migration; it never edits the meaning of an existing one, and it
  never rewrites a conversation's content.

The import of the previous layout (JSONL files, one per session, under the same
directory tree) is the first migration's companion, not a special case in the
program: it runs once, inside a transaction, records that it ran, and only
imports the newest file of each directory. The files themselves are never
deleted or rewritten.

## 5. Invariants

- **One directory, one current conversation.** Declared as a unique index and
  used everywhere: opening, archiving and starting over all read or write
  through it, so a second process cannot create a rival.
- **The store is the only writer.** No front end, and not the agent, touches the
  database; they append through the conversation handle, which owns the
  transaction.
- **Appends are ordered and atomic.** A message is committed on its own, in the
  order it entered the conversation, and a crash loses at most the message being
  written.
- **Archiving never deletes.** Archiving clears the directory's pointer and marks
  the conversation; both rows stay, and `--session` can still open it.
- **Reading a conversation is one snapshot.** The messages and the compaction a
  conversation handle starts from are read together, so a resumed conversation
  cannot see a summary whose messages are missing or a message set that predates
  its compaction.
- **The schema version moves in one direction.** It only increases, only through
  the documented migrations, and never past what the running program understands.
