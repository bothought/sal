# Basic functions

This document is the feature specification of sal: what the program does, from
the outside in. It says nothing about how it is implemented; that is
`design/technical/architecture`.

## 1. Overview

sal is a minimal coding agent. It takes a request in natural language, asks an
LLM what to do, runs the file and shell tools the model asks for, feeds the
results back, and repeats until the model answers without asking for another
tool.

Everything sal does is one of four things: the agent loop; seven built-in tools
(`read`, `bash`, `edit`, `write`, `grep`, `find`, `ls`), the only way sal touches
the machine; three interfaces (browser console, print, JSON), which drive the
same loop and differ only in presentation; and sessions plus configuration
(one stored conversation per working directory, settings from flags, environment
and a config file).

Deliberately out of scope: a terminal UI, extensions, skills, prompt templates,
packages, themes, sub-agents, MCP, plan mode, background bash, session trees,
HTML export, prompt caching, cost accounting, and providers that do not speak the
OpenAI `/chat/completions` protocol.

## 2. The agent loop

A *run* starts with a prompt and ends when the model answers without asking for a
tool; each pass over the model is a *turn*. The request carries the system
prompt, the conversation, and the enabled tool schemas. Answers stream as they
arrive; tool calls run in order and their results are appended before the model
is asked again. Tool failures are fed back as results rather than ending the run.

- **Turn budget:** 32 turns by default (`--max-turns`). Running out ends the run
  with an error and keeps the conversation.
- **Interrupting:** `esc` aborts the run and reports “aborted by user”, not a
  failure; a retry wait is interrupted immediately. A prompt submitted during a
  run is queued.
- **Transient failures:** resets, dropped keep-alives, timeouts, `429` and `5xx`
  retry with exponential backoff (0.5s…10s, four times by default). A
  `Retry-After` header wins. Retrying stops at a permanent failure, a cancelled
  run, or after any answer text was streamed.
- **Context:** the indicator and compaction are
  `design/features/context-management`.

## 3. Tools

All seven tools are enabled by default. `--tools` / `tools` takes a
comma-separated allowlist such as `read,grep,find,ls`; an unknown name is an
error that lists the available tools.

| Tool | What it does |
|---|---|
| `read` | Read a text file with 1-based line numbers, `offset`/`limit` paging. |
| `write` | Create or overwrite a file, creating parent directories. |
| `edit` | Exact text replacement; requires a unique match unless `replaceAll`. |
| `bash` | Run a shell command, returning combined stdout/stderr and the exit code. |
| `grep` | Regex content search with an optional `glob` filter; prints `path:line: text`. |
| `find` | Glob path search; `**` crosses directories, and a pattern without a slash matches file names at any depth. |
| `ls` | List a directory, directories marked with a trailing `/`. |

Paths are relative to the working directory or absolute; `~` expands to the home
directory. A missing required argument is an error. A tool's error is returned to
the model as a failed result rather than ending the run.

Limits that change what the model sees: `read` refuses binaries and directories
and caps at 400 KiB; `bash` keeps 64 KiB of output and kills the process tree on
timeout (default 120s, maximum 600s); `edit` requires a unique match; the search
tools skip VCS and dependency directories, ignore files over 2 MiB, and cap their
results; any tool result is capped at 30 000 characters before it goes back to
the model, with a note when cut. `bash` uses `bash`, or `sh` where bash is
absent; on Windows it uses bash from Git for Windows when present, the system
interpreter otherwise.

## 4. Interfaces

There are three interfaces. None is started implicitly except print mode, which
is the fallback.

### 4.1 Browser console (`--web`)

`sal --web` serves a console over HTTP with server-sent events; the page is
embedded in the binary, so there is no build step or external asset. The default
address is `127.0.0.1:8080`; `--web-addr` selects another.

The console shows user turns, streamed assistant text, each tool call with its
output and duration, errors, and a footer with model, status, context-window use,
token usage, session and turn count. The editor supports the slash commands in
§6.1; `enter` sends, `shift+enter` inserts a newline, `esc` aborts, and a prompt
submitted while the agent is busy is queued.

Authentication is optional and defaults to local trust:

- With no token configured, requests from loopback are accepted without a token.
  Cross-origin browser requests and non-loopback `Host` headers are refused.
- `--web-token` or `$SAL_WEB_TOKEN` sets an explicit token. Every request must
  then present it, including loopback requests.
- Binding a non-loopback address without a token is refused. There is no default
  token file and no persisted secret.

The console is built to be reloaded while the agent works. Every connection
starts from a full snapshot; the session id stays in the URL, so a reload rebuilds
the transcript and `/new` rewrites the URL. A restarted process continues the
directory's conversation and says so on stderr.

### 4.2 Print mode (`--print` / `-p`)

One-shot. The answer streams to stdout; tool activity and errors go to stderr, so
stdout is the answer alone. This is the default when neither `--web` nor
`--mode json` is selected. With no prompt and no piped input, the run fails with
“no prompt given”.

### 4.3 JSON mode (`--mode json`)

The same run as one JSON object per line on stdout: text deltas, messages, tool
calls and results, usage, retries, context figures, compactions, errors, and a
final `done`.

### 4.4 Piped input and `@file`

Non-terminal stdin (up to 4 MiB) is prepended to the prompt, so
`cat main.go | sal -p "review this"` works. An argument starting with `@` embeds
that file (`<file path="...">…</file>`, capped at 512 KiB) ahead of the prompt.

## 5. Sessions

A working directory owns exactly one current conversation. Starting sal there
continues it; there is nothing to list or switch between.

### 5.1 Storage and saving

Conversations live in one SQLite database,
`$XDG_DATA_HOME/sal/sessions/sal.db`. `--session-dir`, `session_dir`, or
`$SAL_SESSION_DIR` move the store. `--no-session` disables storage; the
conversation then lives only in memory and no database is created.

The database enforces one current conversation per canonical directory, makes
each write atomic, and stores the schema version. An older database is migrated
forward and a newer one refused. An older release's JSONL sessions are imported
once, on first start; the newest file of each directory becomes its conversation.

Every message is saved when it enters the conversation: an assistant answer, a
tool result, a compaction. A crash, `kill -9`, or a closed terminal loses at most
the turn in flight; streamed text is saved only as the finished assistant message.
A finished turn is durable before the process exits.

### 5.2 Choosing the directory

`sal` uses its working directory. `--dir <path>` runs with `<path>` as the working
directory, roots the tools there, and selects that directory's conversation.
`--new` archives the stored conversation (never deletes it) and starts fresh.
`--session <id>` opens a specific stored conversation, including an archived one;
`--list-sessions` prints every stored conversation and marks archived ones.

In the console, `/new` starts over here, `/new <path>` continues another
directory's conversation, and `/session` describes the current one. `/new` with
no argument is refused while a turn is in flight because that turn belongs to the
conversation being archived.

### 5.3 What a stored conversation carries

A conversation has an id (16 hex characters, shown as its first 8), a display
name (`--name` / `name`, default the id prefix), the model recorded when it was
created, its working directory, a message count, and an update time. A compacted
conversation says so. Archiving keeps the conversation and marks it archived; it
is no longer what its directory continues but can still be opened by id.

## 6. Commands and configuration

### 6.1 Slash commands

In the browser console:

```
/help  /model [name]  /new [path]  /session  /tools
/context [summary]  /compact [notes]  /clear  /quit
```

`/model <name>` switches the model for the rest of the process. `/session`
describes the current session. `/context` explains the context figure and
`/context summary` prints the active summary. `/compact [notes]` compacts now, or
queues when a turn is in flight. `/clear` empties the transcript without touching
the conversation. `/quit` is not a way to stop the server; close the tab.

### 6.2 Resolution

Every setting resolves as **flag > environment > config file > default**.
`--show-config` prints each resolved value with its source, credentials masked.
`sal --version` prints the version.

### 6.3 Config file

TOML at `$XDG_CONFIG_HOME/sal/config.toml` (`XDG_CONFIG_HOME` defaults to
`~/.config`). JSON is accepted as `config.json` in the same directory; TOML wins
when both exist. `--config` or `$SAL_CONFIG` selects a file. Keys mirror the flag
names: provider, model, base URL, credentials, tool allowlist, turn and retry
budgets, context settings, system prompts, session directory and header, name,
and the two no-* switches.

Unknown TOML keys are warnings, not errors. An `api_key` in a file readable by
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
SAL_WEB_TOKEN                                     web console access token
XDG_CONFIG_HOME / XDG_DATA_HOME                   base directories
```

`SAL_*` wins over the `OPENAI_*` aliases; relative `XDG_*` values are ignored.

### 6.5 Gateway identity

Every request carries `User-Agent: sal/<version>`, plus a per-conversation header
when the endpoint wants one (`x-opencode-session` on `opencode.ai`, or whatever
`session_header` names). The id is the session's, so continuing a conversation
keeps the routing affinity.

### 6.6 System prompt and context files

The default prompt describes sal, the working directory, the OS/arch and the
date. `--system-prompt` replaces it (a value naming an existing file is read from
disk); `--append-system-prompt` adds to it.

Unless `--no-context-files` is given, instruction files are appended: `AGENTS.md`
or `CLAUDE.md` in `$XDG_CONFIG_HOME/sal`, then for each directory from the
filesystem root down to the working directory, the first of
`AGENTS.override.md`, `AGENTS.md`, `CLAUDE.md` that exists.

## 7. Security posture

`bash` runs with the user's privileges and the model chooses what to run. Model
output is untrusted input: prompt injection from a file sal reads can request
arbitrary commands. Use `--tools` to drop `bash`/`write`/`edit` for read-only
work. The console binds to loopback by default; without a token it accepts local
clients but refuses cross-origin browser requests and non-loopback hosts.
Binding a non-loopback address requires a token.

## 8. Explicit non-features

- A terminal UI; the browser console is the only interactive interface.
- Providers other than OpenAI-compatible `/chat/completions`, and `/login`.
- Extensions, skills, prompt templates, packages, themes, sub-agents, MCP, plan
  mode, background bash, session trees, and HTML export.
- Upgrading from inside the console: `sal upgrade` is its own command.
- Prompt caching and cost accounting; approximate context tracking with no
  bundled tokenizer.
- Per-tool result persistence: output is stored, but durations and error flags
  are lost across a restart.
