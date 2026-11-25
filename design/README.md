# sal design documents

This directory is the design specification of sal. It is split so that what sal
does and how sal is built never mix:

```
design/
  features/                  what sal does
    basic-functions/         the current feature set
    context-management/      the context indicator and compaction
    self-upgrade/            replacing the installed binary
  technical/                 how sal is built
    architecture/            the technical architecture
    context-compaction/      the technical design of context-management
    session-store/           the technical design of conversation storage
    self-upgrade/            the technical design of self-upgrade
```

Each item is one folder, and its `README.md` is the entry point. Supporting files
belong inside the item's folder.

- A **feature** document describes every user-visible behaviour: commands,
  settings, screens, stored data, failure and interruption. It is written in the
  vocabulary of the interface, not the code.
- A **technical** document describes the parts, their responsibilities, the data
  between them, and the invariants that keep them honest. It does not contain
  source code, type or function names, or step-by-step algorithms.
- Documents are written and maintained by the developer. AI agents read them but
  do not write them unless the task asks for it.
- Design work and the code implementing it can share a branch, but the document is
  committed before the code that depends on it.

## Index

### Features

| Item | Scope |
|---|---|
| [basic-functions](features/basic-functions/README.md) | The agent loop, the tools, the three interfaces, sessions, configuration, and their guardrails. |
| [context-management](features/context-management/README.md) | The context-window indicator and compaction, automatic or on `/compact`. |
| [self-upgrade](features/self-upgrade/README.md) | `sal upgrade` and `sal upgrade --check`. |

### Technical

| Item | Scope |
|---|---|
| [architecture](technical/architecture/README.md) | Packages, responsibilities, the interfaces between them, concurrency, and testing. |
| [context-compaction](technical/context-compaction/README.md) | The technical design of context-management. |
| [session-store](technical/session-store/README.md) | The database schema, versioning, and the invariants of conversation storage. |
| [self-upgrade](technical/self-upgrade/README.md) | Release discovery, verification, platform replacement, and the release pipeline. |
