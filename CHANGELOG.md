# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Context window tracking and compaction.** The terminal footer and the web
  console now show `ctx used/limit percent` for the next request, distinct from
  the cumulative `↑`/`↓` counters. The figure is measured from the token counts
  the provider reports and estimated (marked `~`) for what was appended since.
  The window resolves from `--context-window` / `context_window` (accepting
  `128k`, `1m`), then the endpoint's model metadata, then a small built-in
  table, then 128k. When the window fills, older messages are replaced by a
  summary — automatically at `--compact-at` (80% by default), or with
  `/compact [notes]`; `--no-auto-compact` turns the automatic trigger off. The
  most recent three turns are kept verbatim, the system prompt and tool
  definitions are never compacted, and the visible transcript is not rewritten.
  `/context` explains the figure and `/context summary` prints the summary. A
  summary that fails leaves the context unchanged, and automatic compaction
  disables itself rather than looping. Session files keep every message and
  record the compaction, so resuming continues from the summary and still sends
  the tool results the compaction had shortened; the session switcher marks a
  compacted session.

- **Session switching.** `/switch` in the TUI opens a picker of the sessions
  stored for the directory (`↑`/`↓`, `enter`, `esc`), `/switch <id>` opens one
  directly, and `/sessions` lists them; switching aborts a turn in flight and
  replays the conversation it switched to. The web console gets a session menu in
  the header and the same `/switch` / `/sessions` commands. `sal
  --list-sessions` prints the stored conversations with the ids `--session`
  accepts, and `sal --continue-all` resumes the most recent session for the
  machine rather than only for the working directory.
- **Retries for transient provider failures.** A reset connection
  (`read: connection reset by peer`), a timeout, a `429`, or a `5xx` is now
  retried with exponential backoff (0.5s to 10s, four attempts by default)
  instead of failing the turn. A server `Retry-After` wins over the backoff, an
  abort during a backoff stops immediately, and a failure after part of the
  answer was streamed is not retried (that text has already been shown). Every
  retry is reported in the TUI, the web console, and print mode. The budget is
  `--max-retries` / `max_retries`.
- The provider HTTP client reuses connections per host and bounds only the wait
  for response headers.

### Fixed

- A dropped connection in the middle of a stream is no longer presented as an
  empty answer: an unfinished stream that delivered nothing is treated as a
  transient failure and retried.
- A long streamed answer is no longer cut off: only the wait for response headers
  is bounded, which a whole-request timeout would have covered too.
- The TUI session picker fits the terminal: entries are clipped and the list is
  windowed to the height, so a narrow or short terminal no longer overflows.
- The context window published by a gateway is now found again. Two faults made
  a real `1M` window fall back to the built-in table (and so to `65k` for a
  `deepseek` model): the metadata read was capped at a failure message's 8 KiB,
  which truncates a full model catalogue into unparseable JSON, and the
  `{"limit":{"context":N}}` nesting that opencode.ai and other
  OpenAI-compatible catalogues use was not read at all. `/context` now reports
  the endpoint's own figure with `from the endpoint`.
- The context window is now resolved at the start of a run, not only inside the
  automatic-compaction check. That check returns early when `auto_compact` is
  off, so a run with it disabled reported the built-in table's figure (and so
  `65k` for a `deepseek` model) for the whole conversation, even though the
  endpoint publishes the real one.
- The web console reuses its access token from
  `$XDG_STATE_HOME/sal/web-token` (overridable with `--web-token` or
  `$SAL_WEB_TOKEN`), so a restarted process keeps serving a page that is already
  open: reload the tab instead of reopening the URL.
- The console URL carries the session id. Loading a URL that names another
  session reopens it, and the transcript is rebuilt from the stored conversation
  — including tool calls and their output, marked with a `restored` note — so a
  reload or a restart shows the whole conversation again. `sal --web -c` resumes
  the latest session for the directory, which is what a rebuild-and-reload loop
  wants.

## [0.1.0] - 2026-10-12

First release.

### Added

- Agent loop over any OpenAI-compatible `/chat/completions` endpoint, with a
  bounded number of turns per prompt and streamed responses.
- Seven built-in tools: `read`, `write`, `edit`, `bash`, `grep`, `find`, `ls`,
  selectable with `--tools`.
- Four interfaces: interactive bubbletea TUI, `--web` browser console, `--print`
  one-shot mode, and `--mode json` for a JSONL event stream.
- JSONL sessions with parent links, grouped per working directory and resumable
  with `-c`; sessions can be named and stored in an explicit directory.
- Configuration from flags, environment variables, and a TOML or JSON file,
  resolved with flags > environment > file > default and inspectable with
  `--show-config`; credentials can be read from a named environment variable.
- `AGENTS.md`/`CLAUDE.md` context files, `--system-prompt` and
  `--append-system-prompt`, and per-conversation session headers for gateways
  that route agent traffic.

[Unreleased]: https://github.com/bothought/sal/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/bothought/sal/releases/tag/v0.1.0
