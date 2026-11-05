# sal design documents

This directory is the design specification of sal: what the program does, and
how it is built. The code is written against these documents, and documents are
updated before (or together with) the code they describe.

These documents are written and maintained by people. AI agents read them but do
not write them — see `AGENTS.md` at the repository root for the rule and for the
rest of the working agreements.

## Layout

```
design/
  features/                  what sal does
    basic-functions/         the current feature set, coarse to fine
    context-management/      the context indicator and compaction
    self-upgrade/            sal downloading and replacing its own binary
  technical/                 how sal is built
    architecture/            the current technical architecture
    context-compaction/      the technical design of context-management
```

The split is strict: a **feature** document answers "what does the user get, and
what does it do", a **technical** document answers "how is that implemented".
Both are written from coarse to fine — an overview first, then the parts, then
the details that matter to whoever changes the code. A document describes the
design as it stands; it is not annotated with whether the code has caught up.

Each item is one folder, and its `README.md` is the entry point:

- One feature item per folder: a feature is never spread across folders, and two
  feature items are never merged into one folder.
- One technical item per folder, with the same rule.
- Supporting files (diagrams, tables, worked examples) go inside the folder of
  the item they belong to.
- Adding an item means adding a folder and a line to the index below.

A feature item and its technical counterpart are two folders, never one: the
feature document states what happens, the technical document how. The two are
separate pieces of work — a feature can be described before the technical design
that implements it exists — so the index names both and marks the one that is
still missing.

Design work and the code implementing it can share a branch, but the document is
committed before the code that depends on it. Merging is the review: a document
on `main` is a document to implement, and the code that implements it needs no
further approval step of its own.

## How much detail each kind of document carries

The two kinds are written at deliberately different depths:

- **Feature documents are detailed, and written for the user.** They describe
  every behaviour the program has: what is displayed, what each command and
  setting does, what is stored, what happens on failure or interruption, and
  what the edge cases are. The vocabulary is the interface's — commands,
  settings, screens, files the user can see — never the code's. A feature
  document should be complete enough that the behaviour can be checked against
  it without reading the source, and the code is expected to match it exactly.
- **Technical documents give the architecture, not the implementation.** They
  say which parts the program is made of, what each part is responsible for, how
  the parts fit together, what data flows between them, which invariants hold,
  and why the structure is that way. They deliberately do not contain source
  code, type or function names, or step-by-step algorithms: those belong in the
  code and its doc comments. A change that only touches such internals needs no
  document change; a change to the parts, their responsibilities, or an
  invariant does.

## Index

### Features

| Item | Scope |
|---|---|
| [basic-functions](features/basic-functions/README.md) | Everything sal currently does: the agent loop, the tools, the four interfaces, sessions, configuration, and the guardrails around them. |
| [context-management](features/context-management/README.md) | The context window: a `used / limit` indicator, and compaction of older messages, automatic when the window fills or on `/compact`. |
| [self-upgrade](features/self-upgrade/README.md) | `sal upgrade` and `sal upgrade --check`: replacing the installed binary with the latest GitHub release, and reporting whether one exists. |

### Technical

| Item | Scope |
|---|---|
| [architecture](technical/architecture/README.md) | Package layout, the responsibility of each part and the interfaces between them, the data formats, the concurrency model, and the testing strategy. |
| [context-compaction](technical/context-compaction/README.md) | The technical design of `features/context-management`: where the policy lives, how the four modes share it, what flows between the loop, the session and the front ends, and which invariants the structure guarantees. |
| [self-upgrade](technical/self-upgrade/README.md) | The technical design of `features/self-upgrade`: release discovery, asset selection and verification, platform replacement, the release pipeline, and the invariants that keep a failed upgrade inert. |

The index lists every item, written or not. An item whose document does not
exist yet is marked *(not written yet)*, so a gap is visible rather than silently
missing.
