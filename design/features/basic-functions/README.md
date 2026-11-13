# Basic functions

This document is the feature specification of sal: what the program does, from
the outside in. It is written coarse to fine — the overview first, then each
area, then the detail that matters to a user or to whoever tests the behaviour.
It says nothing about how any of it is implemented; that is
`design/technical/architecture`.

## 1. Overview

sal is a minimal coding agent for the terminal. It takes a request in natural
language, asks an LLM what to do, runs the file and shell tools the model asks
for, feeds the results back, and repeats until the model answers without asking
for another tool.

Everything sal does is one of four things:

1. **The agent loop** — the turn-by-turn cycle described above.
2. **Seven built-in tools** — `read`, `bash`, `edit`, `write`, `grep`, `find`,
   `ls`, which are the only way sal touches the machine.
3. **Four interfaces** — the interactive terminal UI, the browser console
   (`--web`), one-shot print mode (`--print`), and a machine-readable JSON event
   stream (`--mode json`). All four drive the same agent loop with the same
   tools; they differ only in how the run is presented and controlled.
4. **Sessions and configuration** — each working directory owns one stored
   conversation, continued automatically; the settings that shape a run come
   from flags, environment variables, and a configuration file.

Deliberately out of scope: extensions, skills, prompt templates, packages,
themes, sub-agents, MCP, plan mode, background bash, session trees, HTML export,
prompt caching, cost accounting, and every provider that does not speak the
OpenAI `/chat/completions` protocol. Context-window tracking and compaction are
no longer on this list: they are specified in
`design/features/context-management`. Section 9 repeats the boundary, so a
missing feature is visible here rather than inferred from the code.

## 2. The agent loop

A *run* starts with a prompt and ends when the model answers without asking for
a tool; each pass over the model is a *turn*. Standard agent behaviour, listed
rather than described: the request carries the system prompt, the conversation
and the enabled tool schemas; answers stream as they arrive; tool calls run in
the order asked, their results are appended and the model is asked again;
failures are fed back as tool results instead of ending the run.

**Turn budget.** 32 turns by default (`--max-turns`); running out ends the run
with an error and keeps the conversation.

**Interrupting.** `esc` aborts the run in flight (the UI reports "aborted by
user", not a failure), and a retry wait is interrupted immediately. A prompt
submitted during a run is queued and starts when the run ends.

**Transient failures.** Connection resets, a keep-alive connection the server
closed while idle, timeouts, `429` and `5xx` are retried with exponential backoff
(0.5s…10s, four attempts, `--max-retries`; a negative value disables it). A
`Retry-After` header wins. Retrying stops at a permanent failure, at a cancelled
run, and once part of the answer has been streamed. Only the wait for response
headers is bounded, so a slow but healthy stream is never cut off.

**Context.** Compaction and the context indicator are
`design/features/context-management`.

## 3. Tools

Seven tools are built in, and all seven are enabled by default. `--tools` /
`tools` takes a comma-separated allowlist (e.g. `read,grep,find,ls` for a
read-only run); an unknown name is an error that lists the available tools.

| Tool | What it does |
|---|---|
| `read` | Read a text file with 1-based line numbers (`   12| text`), `offset`/`limit` paging. |
| `write` | Create or overwrite a file completely, creating parent directories. |
| `edit` | Exact text replacement; requires a unique match unless `replaceAll` is set. |
| `bash` | Run a shell command, returning combined stdout/stderr and the exit code. |
| `grep` | Regex content search with an optional `glob` filter; prints `path:line: text`. |
| `find` | Glob path search, where `**` crosses directories; without a slash the pattern matches file names at any depth. |
| `ls` | List a directory, directories marked with a trailing `/`. |

Paths are relative to the working directory or absolute; `~` expands to the home
directory. Arguments are validated (a missing required one is an error), and a
tool's error is returned to the model as a failed result rather than ending the
run.

Guardrails worth knowing, because they change what the model sees: `read` refuses
binaries and directories and caps at 400 KiB; `bash` keeps 64 KiB of output and
kills the whole process tree on timeout (default 120s, max 600s); `edit` demands
a unique match; the search tools skip VCS and dependency directories, ignore
files over 2 MiB, and cap their results; and any single tool result is capped at
30 000 characters before it goes back to the model, saying when it was cut.
`bash` uses `bash`, or `sh` where bash is absent; on Windows it uses bash from
Git for Windows when present, the system interpreter otherwise.

## 4. Interfaces

Four interfaces drive the same loop with the same tools; they differ only in
presentation.

### 4.1 Terminal UI (default)

Used when stdin and stdout are both terminals. A header (version, model,
provider, directory), a scrollable transcript of blocks (user turns, streaming
answers, tool calls with output and duration, notes, errors), a multi-line
editor, and a footer with directory, model, status, `ctx used/limit`, `↑in ↓out`
and the turn count.

Keys: `enter` submits (queues during a run), `alt+enter` / `shift+enter` /
`ctrl+j` insert a newline, `esc` aborts, `pgup`/`pgdn` and `alt+↑/↓` scroll,
`ctrl+l` clears the transcript, `ctrl+c` quits.

### 4.2 Browser console (`--web`)

The same console over HTTP with server-sent events; the page is embedded in the
binary, so there is no build step or external asset. `--web-addr` defaults to
`127.0.0.1:8080`.

- Every request needs the access token, which the startup URL carries and the
  page keeps; the token is persisted in `$XDG_STATE_HOME/sal/web-token` so an
  open tab survives a restart. `--web-token` / `$SAL_WEB_TOKEN` pin a value.
- The session id rides in the URL: reloading reopens that conversation, and a
  restarted server continues the directory's conversation.
- Every (re)connection starts from a full snapshot, so a dropped stream never
  duplicates or loses blocks.

### 4.3 Print mode (`--print` / `-p`)

One-shot: the answer streams to stdout, tool activity and errors go to stderr, so
stdout is the answer alone.

### 4.4 JSON mode (`--mode json`)

The same run as one JSON object per line on stdout: text deltas, tool calls and
results, usage, retries, errors, and a final `done`.

### 4.5 Piped input and `@file`

Non-terminal stdin (up to 4 MiB) is prepended to the prompt, so
`cat main.go | sal -p "review this"` works. An argument starting with `@` embeds
that file (`<file path="...">…</file>`, capped at 512 KiB) ahead of the prompt.

## 5. Sessions

**One directory, one conversation.** A working directory owns exactly one stored
session; opening sal there continues it, and there is nothing to list or switch
between. This keeps the model simple: the identity of a conversation is the
directory it belongs to.

### 5.1 Storage

Conversations live in one SQLite database,
`$XDG_DATA_HOME/sal/sessions/sal.db`. `--session-dir` / `session_dir` /
`$SAL_SESSION_DIR` move the store, and the database file lives in the named
directory. `--no-session` disables storage; the conversation then lives only in
memory and no database is created.

Keeping every conversation in one database, rather than one file each, is what
makes the rest of this section simple: a directory's conversation is a row, its
messages are rows, and the database — not the program — decides who may create
it and what a write costs. Two sal processes in the same directory cannot end up
with two conversations, and a write is never half applied.

The schema carries a version. On startup the program brings the database up to
the version it expects by applying the migrations it knows, one transaction
each; a database written by a *newer* release than the running one is refused
with a clear error instead of being opened and damaged. The version and the
migrations are documented in `design/technical/session-store`.

A conversation stored before this layout — an older release wrote one JSONL file
per session under the same directory tree — is imported once, on first start
with the new storage. The newest file of each directory becomes that
directory's conversation, and every file is left on disk untouched. The import
runs in one transaction, so an interrupted import does not duplicate anything,
and a database that has already imported gains nothing on a later start.

### 5.2 Saving

Every message is saved when it enters the conversation, not at exit: an
assistant answer, a tool result, a compaction, a rename, a model change. A
crash, a `kill -9`, or closing the terminal therefore loses at most the turn
that was in flight. Two consequences are visible and worth stating:

- Text streamed during a turn is not saved incrementally; the finished assistant
  message is saved when the turn completes. A process killed mid-answer keeps the
  completed turns, not the partial text.
- A finished turn is durable even though the process is still running, so a
  second terminal, or a restart, reads the same conversation.

### 5.3 Choosing the directory

`sal` uses the working directory it was started in. Two flags name another one:

| Flag | Effect |
|---|---|
| `--dir <path>` | Run with `<path>` as the working directory, and therefore as the conversation's identity. The tools are rooted there too. |
| `sal --new` | Start over in the current directory: the stored conversation is archived (never deleted) and a fresh one begins. |

`/new` inside a console does the same, and needs the directory to be named when
the console may be showing another one: `/new <path>` switches to the session
stored for `<path>`, creating it when there is none. `/new` with no argument
resets the current directory's conversation.

### 5.4 What a stored conversation carries

Each conversation has an id (16 hex characters, shown as its first 8), a display
name (`--name`/`name`, default the id prefix), a model, the working directory, a
message count and an update time. A compacted conversation says so, because its
active context is shorter than its history. An archived conversation keeps all
of that and is marked as archived; it is no longer what its directory continues
and is not opened by starting sal there, but it is still stored and can be
opened with `--session <id>`.

### 5.5 Commands

| Command | Effect |
|---|---|
| `sal` | Continue this directory's conversation, starting one if there is none. |
| `sal --new` | Archive the stored conversation and start fresh. |
| `sal --dir <path>` | Continue (or start) the conversation for `<path>`. |
| `sal --session <id>` | Open that stored conversation, for recovery or inspection. |
| `sal --list-sessions` | Print every stored conversation on the machine, with its directory; archived ones are marked. |
| `/session` | Describe the current session: id, directory, message count. |
| `/new [path]` | Start over here, or continue `<path>`'s conversation. |

## 6. Commands and configuration

### 6.1 Slash commands

In the terminal UI and the browser console:

```
/help  /model [name]  /new [path]  /session  /tools
/context [summary]  /compact [notes]  /clear  /quit
```

`/model <name>` switches the model for the rest of the process; `/session`
describes the current session; `/context` explains the context figure and
`/context summary` prints the active summary; `/compact [notes]` compacts now
(queueing it if a turn is in flight); `/clear` empties the transcript without
touching the conversation. `/new` is in §5.3. `/quit` closes the terminal UI;
in the browser, closing the tab is the equivalent.

### 6.2 Resolution

Every setting resolves as **flag > environment > config file > default**, and
`--show-config` prints each resolved value with its source, credentials masked.
`sal --version` prints the version.

### 6.3 Config file

TOML at `$XDG_CONFIG_HOME/sal/config.toml`, JSON accepted as `config.json` (TOML
wins); `--config` or `$SAL_CONFIG` names a file. Keys mirror the flag names:

```toml
provider       = "openai"        # the protocol; the URL picks the service
model          = "gpt-4o-mini"
base_url       = "https://api.openai.com/v1"
api_key        = ""              # prefer the environment
api_key_env    = ""              # or name a variable, e.g. GATEWAY_API_KEY
tools          = "read,bash,edit,write,grep,find,ls"
max_turns      = 32
max_retries    = 4               # 0 = default, negative = never retry
context_window = 0               # 0 = resolve it; accepts "128k", "1m"
auto_compact   = true
compact_at     = 80
system_prompt        = ""        # text, or a path to a file
append_system_prompt = ""
session_dir    = ""              # default $XDG_DATA_HOME/sal/sessions
session_header = ""              # "" = auto (see below)
name           = ""              # default session display name
no_session      = false
no_context_files = false
```

Unknown keys are warnings, not errors, and an `api_key` in a file readable by
other users produces a permissions warning.

### 6.4 Credentials and environment

`api_key_env` names a variable to read the key from, checked before the generic
aliases; `--api-key` overrides everything.

```
SAL_API_KEY / OPENAI_API_KEY                      credentials
SAL_BASE_URL / OPENAI_BASE_URL / OPENAI_API_BASE  endpoint
SAL_MODEL / OPENAI_MODEL                          model
SAL_CONFIG                                        config file path
SAL_SESSION_DIR                                   session storage root
SAL_WEB_TOKEN                                     access token for --web
XDG_CONFIG_HOME / XDG_DATA_HOME / XDG_STATE_HOME  base directories
```

`SAL_*` wins over the `OPENAI_*` aliases; relative `XDG_*` values are ignored.

### 6.5 Gateway identity

Every request carries `User-Agent: sal/<version>`, plus a per-conversation
header when the endpoint wants one (`x-opencode-session` on `opencode.ai`, or
whatever `session_header` names). The id is the session's, so continuing a
conversation keeps the routing affinity.

### 6.6 System prompt and context files

The default prompt describes sal, the working directory, the OS/arch and the
date. `--system-prompt` replaces it (a value naming an existing file is read from
disk); `--append-system-prompt` adds to it.

Unless `--no-context-files` is given, instruction files are appended: `AGENTS.md`
or `CLAUDE.md` in `$XDG_CONFIG_HOME/sal`, then for each directory from the root
down to the working directory, the first of `AGENTS.override.md`, `AGENTS.md`,
`CLAUDE.md` that exists.

## 7. Documentation and developer-facing behaviour

- `--help` prints usage, the modes, the commands, every flag, the config-file
  keys, the environment variables, and examples. `sal upgrade` has its own
  `--help`.
- The README documents the same surface for users, and the CHANGELOG records
  notable changes per release.
- The test suite covers the agent loop against a scripted SSE server and the
  real tools against temporary directories; see
  `design/technical/architecture`.

## 8. Security posture (as a feature)

`bash` runs with the user's privileges and the model chooses what to run, so sal
should be used in a directory one is willing to have modified, with model output
treated as untrusted input (a file sal reads can prompt-inject arbitrary
commands). The supported mitigations are `--tools` to drop `bash`/`write`/`edit`
for read-only work, and keeping `--web` on loopback. The web console binds to
`127.0.0.1` by default and authenticates every request with its token; widening
`--web-addr` exposes an interface that can run shell commands to whatever
network it is bound to.

## 9. Explicit non-features

Not implemented, deliberately, and not to be added without a design change:

- Providers other than OpenAI-compatible `/chat/completions` — no native
  Anthropic or Google client, no `/login` OAuth flow. Other vendors are reached
  through a compatible gateway.
- Extensions, skills, prompt templates, packages, themes, sub-agents, MCP, plan
  mode, background bash.
- Upgrading from inside sal's interfaces: `sal upgrade` is a command of its own,
  specified in `design/features/self-upgrade`, and it is not a tool, a slash
  command or an event.
- Multiple conversations per directory, `/switch`, `/fork`, `/clone`, session
  trees, or HTML export. A directory has one conversation. (Compaction is not an
  exception: it changes what the model is sent, not the stored log.)
- Prompt caching and cost accounting. The context window is tracked
  approximately (see `design/features/context-management`): no bundled
  tokenizer, and a small built-in table of model windows rather than a vendor
  matrix.
- `grep` uses Go's regexp rather than ripgrep, and the search tools do not read
  `.gitignore`.
- Per-tool result persistence: tool output is stored in the session, but
  per-call durations and error flags are not, so they are lost across a restart.
