# sal

[![CI](https://github.com/bothought/sal/actions/workflows/ci.yml/badge.svg)](https://github.com/bothought/sal/actions/workflows/ci.yml)

A minimal coding agent written in Go, with a
[bubbletea](https://github.com/charmbracelet/bubbletea) terminal UI.

sal is an agent loop over an LLM with a small set of built-in file/shell tools,
a terminal interface, and JSONL sessions. It deliberately leaves out extension,
skill, package, and provider-matrix machinery.

## What it does

- **Agent loop.** System prompt + conversation history + tool definitions are
  sent to the model; the streamed response is rendered, every requested tool call
  is executed, results are fed back, and the loop repeats until the model answers
  without tool calls (bounded by `--max-turns`, default 32).
- **Seven built-in tools:** `read`, `bash`, `edit`, `write`, `grep`, `find`,
  `ls`.
- **Terminal UI.** Startup header, scrollable transcript showing each tool call
  and its output, multi-line editor, and a footer with directory, model, session
  status, context-window use, and token usage.
- **Four interfaces:** interactive TUI, `--web` browser console, `--print`
  one-shot, and `--mode json` for a machine-readable event stream.
- **JSONL sessions** with parent links, grouped per working directory, resumable
  with `-c`.
- **Context window.** The footer shows `ctx used/limit` for the next request,
  and older messages are summarized automatically when the window fills (or on
  `/compact`), so a long session keeps working.
- **OpenAI-compatible provider** — OpenAI, OpenRouter, Groq, Ollama, llama.cpp,
  vLLM, LM Studio, and anything else speaking `/chat/completions`.

## Install

```bash
go install github.com/bothought/sal@latest   # or:
git clone https://github.com/bothought/sal && cd sal && go build -o sal .
```

Building needs Go 1.27.1 or newer (see `go.mod`); `go build` produces a single
static binary with no runtime dependencies. Release binaries for Linux, macOS
and Windows (amd64 and arm64) are attached to each
[release](https://github.com/bothought/sal/releases).

### Upgrading

An installed release binary can replace itself:

```bash
sal upgrade --check   # is there a newer release? changes nothing
sal upgrade           # download, verify and install it
```

`sal upgrade` takes the latest non-prerelease release, chooses the archive for
this machine (Termux uses the `android/arm64` one — not the Linux build, which
cannot resolve names on Android), checks it against the
release's SHA-256 list, runs it once to confirm it reports the release version,
and only then puts it in place of the running binary. A failure at any step
leaves the installed binary untouched. Packages, sessions and configuration are
not touched; a symbolic link on `PATH` is preserved.

The command needs no API key, no configuration file and no terminal, and it
never downgrades or installs a prerelease. The first word `upgrade` is reserved,
so a prompt that begins with it is written `sal -- upgrade ...`.

A binary installed with `go install` is replaced too, but the module cache is
not: keep using `go install github.com/bothought/sal@latest` for those.

## Configure

Settings resolve with the precedence **flag > environment > config file >
default**, and `sal --show-config` prints the resolved value and the source of
every setting:

```
$ sal --show-config
sal 0.1.0

paths:
  config file   ~/.config/sal/config.toml
  config dir    ~/.config/sal
  session dir   ~/.local/share/sal/sessions
  data dir      ~/.local/share/sal
  state dir     ~/.local/state/sal

settings:
  provider             openai
                         ← default
  model                gpt-4o-mini
                         ← default
  api_key              sk-c…og (masked)
                         ← env $SAL_API_KEY
```

### Config file

TOML at `$XDG_CONFIG_HOME/sal/config.toml` (`XDG_CONFIG_HOME` defaults to
`~/.config`). JSON is also accepted as `config.json` in the same directory, and
TOML wins when both exist. `--config <path>` or `$SAL_CONFIG` selects a
specific file instead.

```toml
# ~/.config/sal/config.toml
model    = "gpt-4o-mini"
base_url = "https://api.openai.com/v1"
api_key  = ""                  # prefer the SAL_API_KEY environment variable
tools    = "read,bash,edit,write,grep,find,ls"
max_turns = 32
max_retries = 4                # 0 = default, negative = never retry a transient failure
context_window = 0             # 0 = resolve it; accepts "128k", "1m"
auto_compact = true            # summarize older messages when the window fills
compact_at = 80                # percent of the window that triggers it

system_prompt        = ""      # text, or a path to a file
append_system_prompt = ""
session_dir = ""               # default $XDG_DATA_HOME/sal/sessions
session_header = ""            # "" = auto (see below)
name        = ""               # default session display name
no_session      = false        # do not persist sessions
no_context_files = false       # do not load AGENTS.md / CLAUDE.md
```

Unknown TOML keys are reported as warnings rather than silently ignored, and a
config file that contains an `api_key` while being readable by other users
produces a permissions warning.

### Credentials without copying secrets

`api_key_env` names the environment variable holding the key, so the secret
stays in one place:

```toml
api_key     = ""
api_key_env = "GATEWAY_API_KEY"
```

The named variable is checked before `SAL_API_KEY` / `OPENAI_API_KEY`; a
literal `api_key` in the file is the last resort, and `--api-key` overrides
everything.

### Gateway identity headers

Some gateways route coding-agent traffic and require clients to identify
themselves and to send a stable per-conversation id. sal always sends
`User-Agent: sal/<version>`, and sends a session id header when the endpoint is
known to want one — currently `x-opencode-session` for `opencode.ai`. Set
`session_header` to name a different header, or to `""` to keep the automatic
choice. The id is the conversation's session id, so resuming with `-c` keeps the
same routing affinity; runs with no stored session get a fresh one.

### Example: a third-party gateway

`examples/config.gateway.toml` shows the shape of a service that speaks the
OpenAI protocol but wants its own credential variable and session header:

```toml
provider       = "openai"                    # the protocol; the URL picks the service
model          = "example-model"
base_url       = "https://gateway.example.com/v1"
api_key        = ""
api_key_env    = "GATEWAY_API_KEY"
session_header = "x-session-id"              # "" = automatic
```

### Environment variables

```
SAL_API_KEY      / OPENAI_API_KEY       credentials
SAL_BASE_URL     / OPENAI_BASE_URL      endpoint (default https://api.openai.com/v1)
SAL_MODEL        / OPENAI_MODEL         model (default gpt-4o-mini)
SAL_CONFIG                              config file path
SAL_SESSION_DIR                         session storage root
SAL_WEB_TOKEN                           access token for --web
XDG_CONFIG_HOME / XDG_DATA_HOME / XDG_STATE_HOME
```

`SAL_*` wins over the `OPENAI_*` aliases. Relative values in the `XDG_*`
variables are ignored, as the XDG specification requires.

### XDG layout

```
$XDG_CONFIG_HOME/sal/config.toml        (or config.json)   configuration
$XDG_DATA_HOME/sal/sessions/                              session files
$XDG_STATE_HOME/sal/web-token                             web console token (0600)
```

## Usage

```bash
sal                                  # interactive
sal "list the go files here"         # interactive with an initial prompt
sal -p "summarize README.md"         # one-shot, prints the response
cat main.go | sal -p "review this"   # pipe into a one-shot run
sal -p @notes.md "extract the todos" # include a file
sal --mode json "inspect this repo"  # JSONL event stream
sal --web                            # browser console on 127.0.0.1:8080
sal --web -c                         # web console continuing the latest session
sal -c                               # resume the most recent session here
sal --tools read,grep,find,ls -p "audit this repo"   # read-only run
```

### Session switching

Conversations are stored per working directory, and a running console can switch
between them without restarting:

- **In the TUI**, `/switch` opens a picker of the sessions stored for the
  directory (newest first, with the current one flagged); `↑`/`↓` choose, `enter`
  opens, `esc` cancels, and a digit jumps to that entry. The list is windowed to
  the terminal height, so it fits at any size. `/switch <id>` opens one directly,
  and `/sessions` lists them. Switching aborts a turn that is in flight and
  replays the conversation it switched to (tool output is not part of the
  history, so those blocks come back as calls without their results).
- **In the web console**, the menu in the header lists the same sessions; the
  footer keeps showing the current one. `/switch <id>` and `/sessions` work
  there too. Because the session id is in the URL, reloading a tab reopens the
  conversation it was showing.
- **From the CLI**, `sal --list-sessions` prints the stored conversations with
  the ids `--session` accepts, and `sal -c` / `sal --continue-all` resume the
  most recent one for the directory or for the whole machine.

```
$ sal --list-sessions
2 session(s) for /work/project:

  9f2c1ab4   the first task · 12 msgs · 10-12 14:05
  3d1e77aa   3 msgs · 10-12 13:58

resume with: sal --session <id>
```

### Web console

`--web` serves the same console in a browser instead of the terminal: the same
agent, tools, sessions, and event stream, with a plain page (no ASCII art).
`--web-addr` picks the listen address; it defaults to `127.0.0.1:8080` and
stays on loopback unless you widen it.

```
$ sal --web
sal web console: http://127.0.0.1:8080/?token=b20e4552…&session=67838d59…
```

The URL carries the access token and the session it was opened in. Every request
must present the token, which is what keeps a page on another origin from
reaching the API through your browser's loopback access, since the console can
run shell commands. The page is served from the binary, so there is no build step
and no extra files.

`xterm`-free transcript: user turns, streamed assistant text, each tool call
with its output and duration, errors, and a footer with model, status,
context-window use, token usage, session, and turns. The editor supports the
same slash commands as the TUI (`/help /model /new /session /tools /context
/compact /clear`); `enter` sends, `shift+enter` inserts a newline, `esc` aborts,
and a prompt submitted while the agent is busy is queued. Server-sent events carry the stream; each reconnect resynchronises
from a full snapshot, so a dropped connection never duplicates or loses blocks.

#### Reloading, and surviving a restart

The console is built to be reloaded while the agent keeps working:

- **The token is persisted**, not regenerated per process. `--web` reuses the
  token in `$XDG_STATE_HOME/sal/web-token` (mode `0600`), creating a random one
  the first time, so a page that is already open keeps working after the process
  is restarted — reload it and it reconnects to the new process. Override with
  `--web-token` or `$SAL_WEB_TOKEN` (both are for development; a fixed token is
  worth no more than its exposure).
- **The session is in the URL.** Loading a URL whose `session` differs from the
  current one reopens that stored session and rebuilds the transcript from it,
  so a reload — or a tab left open across a restart — shows the whole
  conversation again, marked with a `restored` note. `/new` rewrites the URL, so
  the address bar always names the session the tab is showing.
- **A restarted process starts a fresh session** unless you ask it not to, so
  for a rebuild-and-reload loop run `sal --web -c`: it reattaches to the most
  recent session for the directory, and an open tab then finds the same
  conversation either way.

Restoring the transcript reads the session's messages; tool output comes back
whole, while per-call durations are not part of the session file and are lost
across a restart.

### Transient provider failures

Gateways drop connections: a reset (`read: connection reset by peer`), a timeout,
or a `429`/`5xx` response. sal treats those as transient and retries the request
with exponential backoff (0.5s, 1s, 2s, 4s, capped at 10s), four times by
default. A `Retry-After` header from the server wins over the backoff. The TUI,
the web console, and print mode report each retry as it happens:

```
retrying  request to https://… failed: … connection reset by peer
          — attempt 1 of 4 in 0.5s
```

Retrying stops at the first failure that is not transient (a `400`, a bad
response body), and it never retries once part of the answer has been streamed —
that text has already been shown, and a retry would duplicate it. An abort
(`esc`) cancels a backoff wait immediately. Only the wait for response headers is
bounded (60s), so a slow but healthy stream is never cut off. The budget is
`--max-retries` (or `max_retries` in the config file); a negative value disables
retrying, and `0` means the default.

### Editor and keys

| Key | Action |
|---|---|
| `enter` | submit (queues the message if the agent is working) |
| `alt+enter` | newline |
| `esc` | abort the current run, or close the session switcher |
| `pgup`/`pgdn`, `alt+up`/`alt+down` | scroll the transcript |
| `ctrl+l` | clear the transcript |
| `ctrl+c` | quit |

### Context window and compaction

The footer carries a context segment between the status and the cumulative token
counters:

```
~/work/sal   gpt-4o-mini · ready · ctx 12.4k/128k 10% · ↑120k ↓8.2k · 3 turns
```

`used` is the size of the *next* request — system prompt, tool definitions, and
the active conversation — while `↑`/`↓` stay the cumulative spend. A `~` marks
an estimated figure: the provider's reported token counts make it exact after
each response, and anything appended since is estimated until the next one.

The limit is resolved from `--context-window`, then the endpoint's model
metadata when it publishes one, then a small built-in table, then a conservative
128k. `/context` prints the breakdown and the source of each number.

As the window fills, older messages are replaced by a summary, first
automatically at 80% (or `--compact-at`), and on demand with:

```
/compact [what to keep]
```

The most recent 3 turns are kept verbatim, the system prompt and tool
definitions are never touched, and the summary is labelled as a record rather
than instructions. The visible transcript is not rewritten: it stays your
record of what happened. The session file keeps every message, and resuming
continues from the summary. `/context summary` prints what the model was told
about the compacted part. `--no-auto-compact` leaves compaction to you; the
footer still warns as the window fills, and a request that is too large then
fails with the provider's error.

A compaction in progress shows `compacting…` in the status, and a prompt
submitted meanwhile is queued. A failed compaction leaves the context unchanged
and says so; it never blocks the run. With automatic compaction off because it
could not free enough of the window, the notification says so rather than
failing silently later.

### Slash commands

```
/help      /model [name]   /new      /session
/switch [id]              /sessions /tools
/context [summary]        /compact [notes]
/clear     /quit
```

`/context` prints the breakdown: the model, where the window came from, used and
its parts (system prompt, tools, summary, recent messages), headroom, the
automatic-compaction setting and the last measured request. `/compact [notes]`
compacts immediately when idle and queues after the current turn otherwise; the
notes are passed to the summarization as what to preserve.

### Context files

`AGENTS.md` or `CLAUDE.md` in the config directory (`$XDG_CONFIG_HOME/sal`) is
loaded first. Then, for each directory from the filesystem root down to the
working directory, the first of `AGENTS.override.md`, `AGENTS.md`, or
`CLAUDE.md` that exists is added, so the working directory wins. Disable with
`--no-context-files`. `--system-prompt` replaces the default prompt;
`--append-system-prompt` adds to it.

## Tools

| Tool | Purpose |
|---|---|
| `read` | Read a text file with 1-based line numbers, `offset`/`limit` paging |
| `write` | Create or overwrite a file, creating parent directories |
| `edit` | Exact text replacement; requires a unique match unless `replaceAll` |
| `bash` | Run a shell command, capture stdout+stderr and exit code, timeout |
| `grep` | Regex content search, `glob` filter, skips VCS/dependency dirs |
| `find` | Glob path search, `**` crosses directories |
| `ls` | List a directory, directories marked with `/` |

Guardrails: tool results are capped before being fed back to the model, `read`
refuses binary files, `bash` keeps at most 64 KiB of output (head and tail),
runs with a timeout (default 120s, max 600s) and kills the whole process group
on timeout, and `grep`/`find` skip `.git`, `node_modules`, `vendor`, and
similar directories.

## Security

`bash` executes commands with your user's privileges and the model decides what
to run, so run sal in a directory you are willing to have modified. Model output
is untrusted input: prompt injection from files sal reads can request arbitrary
commands. Use `--tools` to drop `bash`/`write`/`edit` for read-only work, and
`--web-addr` to keep the console on loopback.

The web console binds to `127.0.0.1` by default and authenticates every request
with the token in its URL; widening `--web-addr` exposes an interface that can
run shell commands, so put it behind your own access control. The token is
reused from `$XDG_STATE_HOME/sal/web-token` so a restarted process keeps serving
pages that are already open — that file is as sensitive as the console itself
(it is created mode `0600`), and `--web-token`/`$SAL_WEB_TOKEN` trade that
convenience for a fixed, guessable value.

Report vulnerabilities privately rather than in a public issue — see
[SECURITY.md](SECURITY.md).

## Development

```bash
go test ./...          # full suite
go test -race ./...    # race detector (Linux/macOS)
go vet ./...
gofmt -l .             # must print nothing
```

The suite runs the real agent loop against a scripted SSE server over loopback
and executes the real tools against temp directories, so the tool-call round
trip, error handling, session persistence, and the TUI's event handling are
covered end to end.

Rebuilding while the web console runs is the normal loop: `go build -o sal .`
replaces the binary, and restarting the process leaves an open browser tab
usable — reload it and the token, the session and the transcript all come back
(see *Reloading, and surviving a restart* above).

See [CONTRIBUTING.md](CONTRIBUTING.md) for the ground rules, and
[CHANGELOG.md](CHANGELOG.md) for what changed in each release. Security issues
should go through a private advisory, as described in [SECURITY.md](SECURITY.md).

## Limitations

Deliberate, given "basic functions only":

- Only the OpenAI-compatible provider is implemented; there is no native
  Anthropic or Google client and no `/login` OAuth flow. Other vendors are
  reachable through a compatible gateway.
- No extensions, skills, prompt templates, packages, themes, sub-agents, MCP,
  plan mode, or background bash.
- No session tree navigation (`/tree`, `/fork`, `/clone`) or HTML export;
  sessions are linear and resumable only. Compaction is not an exception: it
  changes what the model is sent, not the stored session, which keeps every
  message.
- No prompt caching or cost accounting; the footer reports token counts only.
- Context tracking is approximate: no bundled tokenizer, and a small built-in
  table for model windows rather than a vendor matrix.
- `grep` uses Go's `regexp` rather than ripgrep, and neither search tool reads
  `.gitignore`.
- `sal upgrade` verifies the release's checksums but not a signature, and it
  only installs the latest release: no downgrade, no pinned or prerelease
  version. It replaces the binary and nothing else.

## License

Copyright (C) 2026 the sal authors.

sal is free software: you can redistribute it and/or modify it under the terms
of the GNU General Public License as published by the Free Software Foundation,
either version 3 of the License, or (at your option) any later version.

sal is distributed in the hope that it will be useful, but **without any
warranty**; without even the implied warranty of merchantability or fitness for
a particular purpose. See the [GNU General Public License](LICENSE) for more
details.

The full text is in [LICENSE](LICENSE). Third-party Go modules keep their own
licenses (all MIT/BSD); see `go.sum` and each module's `LICENSE` file.
