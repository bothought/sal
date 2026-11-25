# AGENTS.md

Notes for AI coding agents (and for people) working in this repository.

## What this project is

sal is a minimal coding agent written in Go. It is an agent loop over an LLM
with a small set of built-in file/shell tools, a browser console, SQLite
sessions, and an OpenAI-compatible provider.

The deliberate scope is "basic functions only": one provider protocol
(`/chat/completions`), seven tools (`read`, `bash`, `edit`, `write`, `grep`,
`find`, `ls`), three interfaces (browser console, `--print`, `--mode json`).
There is no terminal UI. There are no extensions, skills, packages, sub-agents,
MCP, plan mode, or vendor matrix. See the README's *Limitations* section.

## Repository map

```
main.go                  CLI entry point: flags, config resolution, mode
                         selection, print/JSON/web wiring
internal/agent/          the agent loop: request → stream → tools → repeat
internal/provider/       OpenAI-compatible chat-completions client (SSE)
internal/tools/          the seven built-in tools and the tool registry
internal/session/        SQLite conversation storage, listing, resume
internal/web/            HTTP + SSE console, index.html served from the binary
internal/config/         config file loading and XDG directory resolution
internal/upgrade/        replacing the binary with a newer release
```

Dependency direction: `main` wires everything together; `internal/*` packages
depend on `internal/agent` (for the shared message/event/tool types) and
`internal/config`, never on `main` or on each other's internals.

## Build and test

```bash
go build ./...         # or: go build -o sal .
go vet ./...
go test ./...
go test -race ./...    # Linux/macOS
gofmt -l .             # must print nothing
```

CI runs the suite natively on Linux, macOS and Windows, and the race detector on
Linux and macOS. Keep tests portable: no `/tmp` literals, no assumptions about
the host shell beyond what the `bash` tool documents, and no test that depends on
a file mode a git checkout (or Windows) cannot carry — assert what the program
did, not what the host's filesystem reports.

Tests come with the change: the suite runs the real agent loop against a
scripted SSE server and the real tools against temp directories, so an end-to-end
test for a new path is usually straightforward.

Style: standard library first, small interfaces, doc comments on exported
identifiers, errors that name what failed. One logical change per commit.

## Design documents

`design/` holds the project's design documentation. It is the specification the
code is written against, and it is split so that "what sal does" and "how sal is
built" never mix:

```
design/README.md          index of the design docs and the folder conventions
design/features/          what sal does — one folder per feature item
  basic-functions/        the current feature set, coarse to fine
  context-management/     the context indicator and compaction
  self-upgrade/           sal downloading and replacing its own binary
design/technical/         how sal is built — one folder per technical item
  architecture/           the current technical architecture
  context-compaction/     how context-management is built
  session-store/          the SQLite conversation store
  self-upgrade/           how self-upgrade is built
```

Each item is a folder whose `README.md` is its entry point; supporting notes or
diagrams live beside it, inside the same folder.

### How much detail each kind of document carries

- **Feature documents are written for the user.** They describe every behaviour
  the program has: what is displayed, what each command and setting does, what is
  stored, what happens on failure or interruption, and the edge cases. The
  vocabulary is the interface's, never the code's.
- **Technical documents give the architecture, not the implementation.** They
  say which parts the program is made of, what each is responsible for, how the
  parts fit together, what data flows between them, which invariants hold, and
  why the structure is that way. They deliberately do not contain source code,
  type or function names, or step-by-step algorithms.

Keep both kinds short and specific. Remove repetition, rationale essays and
details that are obvious from the code or the help text.

### Rules for AI agents

- **`design/` is written by the developer.** An agent writes under `design/` only
  when the task asks for it; otherwise the documents are read-only, and a
  document that looks wrong or incomplete is reported in the response instead of
  being edited. Documents are never changed on the agent's own initiative.
- **Code follows design.** Every behaviour in the code must be described by some
  document under `design/`. When a design doc is added or changed, the matching
  code change belongs to the same piece of work, so the two never drift apart.
  When a feature document and the code disagree, the document wins and the code
  is changed; when a technical document names an internal that the code has since
  restructured, the document is the thing to correct.
- **Nothing that design does not describe.** Do not implement a feature, flag,
  setting, tool, or mode that the design documents do not mention.
- **Commit the document first.** Design work and the code that implements it can
  share one branch, but the document is committed before the code that depends on
  it. Merging is the review: a document on `main` is a document to implement.

Because sal loads `AGENTS.md` (and `CLAUDE.md`) as context files, this file is
also the instruction set an agent run by sal reads from its own working
directory.

## Where to look first

- Feature change → the matching folder under `design/features/`, then the README
  section that documents it.
- Technical change → `design/technical/architecture/README.md`, then the package
  the change touches.
- User-visible change → update README.md and CHANGELOG.md as well.
