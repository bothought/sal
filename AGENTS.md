# AGENTS.md

Notes for AI coding agents (and for people) working in this repository.

## What this project is

hal is a minimal coding agent written in Go, driven from the terminal. It is an
agent loop over an LLM with a small set of built-in file/shell tools, a
[bubbletea](https://github.com/charmbracelet/bubbletea) terminal UI, a browser
console, JSONL sessions, and an OpenAI-compatible provider.

The deliberate scope is "basic functions only": one provider protocol
(`/chat/completions`), seven tools (`read`, `bash`, `edit`, `write`, `grep`,
`find`, `ls`), four interfaces (interactive TUI, `--web`, `--print`,
`--mode json`). There are no extensions, skills, packages, sub-agents, MCP,
plan mode, or vendor matrix. See the README's *Limitations* section.

## Repository map

```
main.go                  CLI entry point: flags, config resolution, mode
                         selection, print/JSON/web wiring
internal/agent/          the agent loop: request → stream → tools → repeat
internal/provider/       OpenAI-compatible chat-completions client (SSE)
internal/tools/          the seven built-in tools and the tool registry
internal/session/        JSONL session storage, listing, resume
internal/tui/            bubbletea terminal UI
internal/web/            HTTP + SSE console, index.html served from the binary
internal/config/         config file loading and XDG directory resolution
```

Dependency direction: `main` wires everything together; `internal/*` packages
depend on `internal/agent` (for the shared message/event/tool types) and
`internal/config`, never on `main` or on each other's internals.

## Build and test

```bash
go build ./...         # or: go build -o hal .
go vet ./...
go test ./...
go test -race ./...    # Linux/macOS
gofmt -l .             # must print nothing
```

CI runs vet, the tests, and the race detector on Linux. macOS and Windows are
part of the target set but are not in the matrix yet, to keep iteration fast;
keep tests portable anyway, so turning them back on is a workflow change: no
`/tmp` literals, no assumptions about the host shell beyond what the `bash` tool
documents, and no test that depends on a file mode a git checkout cannot carry.
Tests come with the change: the suite runs the real agent loop against a
scripted SSE server and the real tools against temp directories, so an end-to-end
test for a new path is usually straightforward.

Style: standard library first, small interfaces, doc comments on exported
identifiers, errors that name what failed. One logical change per commit.

## Design documents

`design/` holds the project's design documentation. It is the specification the
code is written against, and it is split so that "what hal does" and "how hal is
built" never mix:

```
design/README.md          index of the design docs and the folder conventions
design/features/          what hal does — one folder per feature item
  basic-functions/        the current feature set, described coarse to fine
  context-management/     the context indicator and compaction
design/technical/         how hal is built — one folder per technical item
  architecture/           the current technical architecture
  context-compaction/     how context-management is built
```

Each item is a folder whose `README.md` is its entry point; supporting notes or
diagrams live beside it, inside the same folder. New items are added as new
folders and listed in the parent index. Feature items and technical items are
never merged into one document, and two unrelated feature (or technical) items
are never merged into one folder.

### How much detail each kind of document carries

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

Design work and the code implementing it can share one branch, but the document
is committed before the code that depends on it. Merging is the review: a
document on `main` is a document to implement, and the code implementing it needs
no further approval step of its own.

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
  restructured, the document is the thing to correct, since internals are
  described in the code itself.
- **Nothing that design does not describe.** Do not implement a feature, flag,
  setting, tool, or mode that the design documents do not mention. If the code
  needs new behaviour, the design has to describe it first.
- **Commit the document first.** Design work and the code that implements it can
  share one branch, but the document is committed before the code that depends on
  it, so the code in a change is always backed by a document that already exists.
  Merging is the review: a document on `main` is a document to implement, and the
  code implementing it needs no further approval step of its own.

Because hal itself loads `AGENTS.md` (and `CLAUDE.md`) as context files, this
file is also the instruction set an agent run by hal reads from its own working
directory.

## Where to look first

- Feature change → the matching folder under `design/features/`, then the README
  section that documents it for users.
- Technical change → `design/technical/architecture/README.md`, then the package
  the change touches.
- User-visible change → update README.md and CHANGELOG.md as well.
