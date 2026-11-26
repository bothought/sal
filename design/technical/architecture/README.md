# Architecture

This document describes which parts sal is made of, what each owns, how they fit
together, and the invariants that keep them honest. What sal does is
`design/features/basic-functions`.

## 1. Shape

sal is one Go module producing one static binary. The entry point is the only
place that wires parts together; everything else lives under `internal/`:

```
main                    CLI: flags, precedence, mode selection, wiring
internal/agent          the agent loop
internal/provider       OpenAI-compatible streaming client
internal/tools          the seven built-in tools and their registry
internal/session        conversation storage (SQLite)
internal/web            HTTP + SSE console, with its page embedded
internal/config         config file and XDG path resolution
internal/upgrade        replacing the binary with a newer release
```

Dependency direction:

```
main ──▶ agent ◀── provider
  │        ▲
  ├──▶ tools ┘
  ├──▶ session ──▶ agent, config
  ├──▶ web  ──▶ agent
  ├──▶ config
  └──▶ upgrade   (independent of everything else)
```

The agent knows nothing about the other packages. It defines the neutral types a
conversation, a tool and a provider answer are made of, and the two interfaces
everything else implements or consumes: a provider and a tool.

The web console is an adapter: it declares the small slice of the agent it
drives, does not depend on the concrete loop, and is testable against a scripted
fake. It does not compute a figure, decide what the model is sent, or touch the
database.

Sessions are injected. The entry point builds one set of callbacks (describe,
identify, start over, open another directory's conversation) and hands them to
the console, so `/new` has one meaning and the console never learns the file
layout.

The loop reports everything as a stream of events, and the console is a fold over
that stream. Anything displayed is an event first; nothing reaches back into the
loop to draw itself.

## 2. The agent loop

The loop owns the conversation, the enabled tools, the system prompt, the run
limits, the retry policy and the context policy. It drives one prompt to an
answer: stream an assistant message, append it, run every requested tool, append
the results, and repeat until the model answers without a tool or the turn budget
runs out.

History grows in one place, so persistence and the event stream cannot disagree
with the conversation. The request is built from a single view of what the model
is sent; after a compaction that view is not the whole history.

The event stream is the contract: text deltas, tool activity, per-turn and
per-request accounting, retries, context figures, compactions, completion and
failure.

Tool execution is a lookup by name and a run. A failure is reported to the model
as a result rather than ending the run: an unknown tool, a rejected path, or a
non-zero exit is something the model can correct. Results are capped before they
enter the conversation.

### 2.1 Retry policy

The policy lives in the loop; the provider classifies a failure as transient or
permanent. A transient failure is waited out with exponential backoff or the
server's `Retry-After`, up to the configured budget. A permanent failure, an
exhausted budget, a cancelled run, or a failure after answer text was streamed is
returned unchanged. The wait is interruptible. A summarization request uses the
same policy but reports no retry notices.

## 3. Provider

The provider owns one protocol: an OpenAI-compatible chat-completions endpoint,
streamed. It translates between neutral messages and the wire format, assembles
credentials, user agent and the optional session-affinity header, reassembles
fragmented text and tool calls, attaches usage to the answer, and classifies
failures.

“Transient” means a transport error that looks like a reset or timeout, or a
`408`/`429`/`5xx` status, with a retry hint when the server sent one. Everything
else is permanent.

The provider also holds the model name and session id that runtime switching can
change, exposed through optional interfaces the agent uses only when the provider
implements them. That is the extension point: a provider that cannot switch
models is never asked to.

The client has no overall timeout, because that would truncate a long answer;
only the wait for response headers is bounded. A failed response body is drained
after its message is read so a retry can reuse the connection.

## 4. Tools

The tools package is a flat registry plus one implementation per tool. Each tool
declares a name, a description and a JSON Schema, and runs a call against a
working directory.

Shared behaviour belongs to the registry:

- an allowlist selects the enabled tools, and an unknown name is one error
  listing the valid ones;
- paths are resolved (home expansion, then the working directory) and results
  show paths relative to the working directory.

The shell tool runs a command in a working directory with a bounded output buffer
that keeps a head and a tail, under a timeout; on expiry it kills the whole
process tree. A non-zero exit appears in the result text and as a tool error.

The read/search tools share a list of directories they never descend into and cap
their results, saying when they truncated.

## 5. Sessions

The session package owns conversation storage; the schema and invariants are
`design/technical/session-store`.

- **A conversation is a row.** One embedded database holds the conversation, its
  messages in append order, and its compaction. A message embeds the neutral
  message type, so a stored conversation replays into the agent.
- **A working directory names exactly one conversation**, declared as a unique
  index on the canonical directory.
- **Writes happen when the conversation changes**, one transaction per change.
  Archiving keeps the conversation and clears the directory's pointer; nothing is
  deleted. The schema is versioned.
- **The previous layout is imported once** from JSONL files, and the store is the
  only writer: no front end and not the loop touches the database.

A compaction is one more row, recording the summary, the last message it covers,
and the tool results it shortened, all by message identity. That is what makes a
resumed conversation continue from the summary instead of replaying the log.

## 6. Web console

The console is an HTTP server with a single embedded page.

- **Transport.** One server-sent-event stream; the rest is JSON. Every connection
  starts with a full snapshot, so a client that fell behind can reconnect. The
  backlog is bounded and an idle comment keeps connections alive.
- **State.** The transcript, run state, counters and subscriber set live behind
  one mutex. The same fold produces the stored transcript and the page delta.
- **Runs.** A prompt starts a run or is queued. A run has a cancellable context;
  finishing starts whatever was queued. Abort marks the run so its failure is not
  reported.
- **Auth.** A configured token is required everywhere. Without one, the console
  accepts loopback clients only, rejects cross-origin browser requests and
  non-loopback hosts, and refuses a non-loopback bind. `--no-web-auth` disables
  all of that for a deployment behind a trusted forward-auth proxy, and permits
  any bind address.
- **Switching conversation** aborts the run in flight, waits for it to finish,
  swaps the session, and rebuilds the transcript. The page keeps the session id
  in its URL.

## 7. Configuration and startup

- Directory resolution follows XDG, ignoring relative values and falling back to
  home-relative defaults.
- File loading prefers an explicit path, then TOML, then JSON. Unknown keys and a
  world-readable API key are warnings.
- Precedence is applied once per setting: flag, then environment, then config
  file, then default. Every value records its source for `--show-config`.
- Startup parses and validates flags, loads configuration, answers informational
  flags, reads a piped prompt, builds the provider/tools/system prompt, installs
  the session, and selects the interface.
- `--web` serves the console, `--mode json` streams events, and everything else is
  print mode.

## 8. Concurrency

- One run at a time. The run executes on its own goroutine; the console never
  blocks on the model.
- Communication is one-way: the loop emits events, and the console folds them
  under its mutex.
- Shared mutable state is small and guarded.
- Cancellation travels through the run's context, reaching the provider call, a
  retry wait, and a spawned shell.

## 9. Testing

The suite runs the real loop against a scripted SSE server and the real tools
against temporary directories. Per-package tests use the smallest real thing
available: a fake server for provider classification, temp directories for tools,
real files for sessions, and injected waits for retry policy. The console is
tested against a scripted fake agent, without a socket or browser.

No test uses fixed temporary paths, assumes a host shell beyond what the shell
tool documents, or depends on a file mode a checkout cannot carry. The suite runs
natively on every release platform.

## 10. Extension points

A new provider implements the provider interface; runtime mutation of model or
session id is optional. A new tool declares its schema and run and joins the
registry. A new front end consumes the same agent interface and event stream.
Everything else is out of scope until a design document says otherwise.
