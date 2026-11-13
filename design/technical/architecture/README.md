# Architecture

This document is the technical description of sal: which parts the program is
made of, what each part is responsible for, how the parts fit together, and
which invariants keep them honest.

It describes the architecture, not the implementation. Locating a behaviour in
the code, and the shape of the code inside a part, are the code's own business:
every package documents itself, and the names there are the ones to read. What
sal *does* is `design/features/basic-functions`; the two documents are
deliberately separate, and neither absorbs the other.

## 1. Shape of the program

sal is a single Go module producing one static binary. One package is the entry
point and the only place that wires the parts together; everything else lives
under `internal/` in a package whose name says what it owns:

```
main                    CLI: flags, precedence, mode selection, wiring
internal/agent          the agent loop (the core of the program)
internal/provider       OpenAI-compatible streaming client
internal/tools          the seven built-in tools and their registry
internal/session        conversation storage (SQLite)
internal/tui            terminal UI
internal/web            HTTP + SSE console, with its page embedded
internal/config         config file and XDG path resolution
internal/upgrade        replacing the binary with a newer release
```

Dependency direction is a straight line with the agent at the centre:

```
main ──▶ agent ◀── provider
  │        ▲
  ├──▶ tools ┘
  ├──▶ session ──▶ agent, config
  ├──▶ tui  ──▶ agent, session
  ├──▶ web  ──▶ agent, session
  ├──▶ config
  └──▶ upgrade   (independent of everything else)
```

The agent knows nothing about the other packages. It defines the neutral types
that a conversation, a tool and a provider answer are made of, and the two
interfaces everything else implements or consumes: a provider, and a tool.
Everything that understands a vendor protocol, a filesystem, a terminal or a
socket sits outside it.

Three consequences are worth stating because they are the reason the program
stays small:

- **The front ends are adapters, not owners.** Each declares the small slice of
  the agent it drives, rather than depending on the concrete loop, so both are
  testable against a scripted fake. Neither front end computes a figure, decides
  what the model is sent, or touches the database.
- **Sessions are injected.** The main package builds one set of callbacks
  (describe, identify, start over, open another directory's conversation) and
  hands the same ones to both front ends, so `/new` means the same thing in each
  and no front end learns the file layout.
- **One observation channel.** The loop reports everything that happens as a
  stream of events, and every interface is a fold over that stream. Anything an
  interface displays is an event first; nothing needs to reach back into the
  loop to draw itself.

## 2. The agent loop

The loop is the core, and its responsibilities are:

- hold the conversation, the enabled tools, the system prompt, the run limits,
  the retry policy, and the context policy;
- drive one prompt to an answer: stream an assistant message, append it, run
  every tool call it requested, append the results, and repeat until the model
  answers without asking for a tool or the turn budget runs out;
- make history grow in exactly one place, so persistence and the event stream
  can never disagree with the conversation;
- build the request — system prompt, then the active conversation, plus the
  tool schemas — from a single view of what the model is sent, which after a
  compaction is not the same as the conversation it holds;
- apply the retry policy to provider calls, and the context policy to the
  request it is about to send.

The event stream is the contract: text deltas, tool activity, per-turn and
per-request accounting, retries, context figures, compactions, completion and
failure. A front end that folds those events correctly needs to know nothing
else.

Tool execution is a lookup by name and a run, with a failure reported back to
the model as a result rather than ending the run: an unknown tool, a rejected
path or a non-zero exit is something the model can correct. Results are capped
before they enter the conversation, so one noisy command cannot consume the
window by itself.

### 2.1 Retry policy

The policy lives in the loop, not in the provider: the provider classifies a
failure as transient or permanent, the loop decides whether to try again.

- A transient failure (a reset connection, a timeout, a `429`, a `5xx`) is
  waited out with exponential backoff, or the server's own `Retry-After` when
  that is longer, and retried up to the configured budget.
- A permanent failure, an exhausted budget and a cancelled run are returned to
  the caller unchanged.
- Nothing is retried once part of the answer has been shown, because the retry
  would duplicate text the user has already read.
- The wait is interruptible: an abort during a backoff returns immediately.
- A summarization request is subject to the same policy but reports nothing of
  its own, so a retried summary never looks like a retried turn.

## 3. Provider layer

The provider owns one protocol: an OpenAI-compatible chat-completions endpoint,
streamed. Its responsibilities are to translate between the neutral message
types and the wire format, to assemble the request headers (credentials,
user agent, and the session-affinity header when one is configured), to consume
the event stream and reassemble text and tool calls that arrive in fragments, to
attach the reported usage to the answer, and to classify what went wrong when a
request fails.

Classification is the part the rest of the program depends on: "transient" is a
transport error that looks like a reset or a timeout, or a `408`/`429`/`5xx`
status, and it carries a retry hint when the server sent one; everything else,
including a malformed URL, a TLS failure and a cancelled run, is permanent.

The provider also holds the state that runtime switching changes — the model
name and the session id — and exposes it through optional interfaces that the
agent reaches for only when the provider implements them. That is the extension
point: a provider that cannot switch models is simply never asked to, and the
core interface stays small.

Streaming needs care in two places, and both are design decisions: the client
has no overall timeout, because that would truncate a long answer (only the time
to first byte is bounded), and a failed response body is drained after its
message is read, so a retry can reuse the connection.

## 4. Tools

The tools package is a flat registry plus one implementation per tool. Every
tool declares a name, a description and a JSON Schema for its arguments, and
runs a call against a working directory.

Shared behaviour belongs to the registry rather than to each tool:

- an allowlist selects which tools are enabled, and an unknown name is one error
  listing the valid ones;
- paths the model supplies are resolved (home expansion, then the working
  directory) and paths in results are shown relative to the working directory,
  which is what keeps results short and readable.

The shell tool is the only one that spawns a process. It runs a command in a
working directory with a bounded output buffer that keeps a head and a tail with
a marker for what was dropped, under a timeout; on expiry it kills the whole
process group, so a shell's children cannot outlive the call. A non-zero exit is
reported both in the result text and as a tool error, because the model should
see both.

The read/search tools share a list of directories they never descend into (VCS
metadata, dependency trees, caches) and cap their results, saying when they
truncated.

## 5. Sessions

The session package owns conversation storage; the schema, the versioning rules
and the invariants are `design/technical/session-store`. What matters at this
level is the shape it puts between the loop and the disk:

- **A conversation is a row, not a file.** Every conversation lives in one
  embedded database: the conversation, its messages in append order, and its
  compaction. A message embeds the neutral message type, so a stored conversation
  replays straight into the agent.
- **A working directory names exactly one conversation**, declared as a unique
  index on the canonical directory. Canonicalising first (resolving links) is
  what keeps one directory from appearing twice, and the database — not the
  program — is what refuses a second conversation for it.
- **Writes happen when the conversation changes**, not at exit, one transaction
  per change. A process that dies between turns loses nothing, and a process
  that dies mid-write leaves the previous state rather than a fragment. Streamed
  text is not written incrementally; the finished message is.
- **Archiving keeps the conversation.** Starting over clears the directory's
  pointer and marks the conversation archived; nothing is deleted, and the
  conversation can still be opened by id.
- **The schema is versioned.** The store applies its known migrations on start
  and refuses a database written by a newer release.
- **The previous layout is imported once**, from the JSONL files an older
  release wrote, without modifying them.
- The store is the only writer: no front end and not the loop touches the
  database, and a front end knows nothing about the layout.

A compaction is one more row, and it is what makes a resumed conversation
continue from the summary instead of replaying the whole history. Because the
history stays complete, the row has to say which messages the summary replaces
and which tool results inside the kept tail were shortened; both are recorded by
message identity rather than position, so they survive any change in how
messages are counted.

## 6. Front ends

Both front ends follow the same shape: they own the transcript and the display
state, forward a prompt to the loop on a separate goroutine, fold the event
stream into that state, and keep a cancellable handle so an abort is prompt.
Neither decides anything about the conversation.

### 6.1 Terminal UI

A bubbletea model with a scrolling transcript, an editor and a spinner.

- **The run runs off the update loop.** The loop runs on its own goroutine and
  forwards events into a buffered channel, so the terminal stays responsive and
  the spinner keeps ticking while the model streams.
- **The transcript is a list of blocks**, folded from events: a text delta opens
  or extends an assistant block, a tool call opens a block that its result
  fills in, and retries, notifications and failures are blocks of their own.
- **Aborts are tracked by the front end.** A cancelled run arrives as text, so
  the model remembers that it asked for the abort rather than re-deriving it
  from the error.
- **A run belongs to the conversation it started in.** The session id is
  captured at launch and compared when the run finishes, so a turn from a
  session the console has left cannot land in the one it is showing.
- **Layout** gives the transcript the height left over by the editor and the
  footer, and the footer carries the working directory, the model, the status,
  the context segment and the cumulative counters.

### 6.2 Web console

The same console as an HTTP server, with a single embedded page so there is no
build step and no external asset.

- **Transport.** One endpoint is a server-sent-event stream; everything else is
  JSON. Every connection starts with a full snapshot, so a client that fell
  behind can simply reconnect. The subscriber backlog is bounded and a client
  that cannot keep up is dropped; a periodic comment keeps idle connections
  alive through proxies.
- **State.** The transcript, the run state, the counters and the subscriber set
  live behind one mutex, and the same fold produces both the stored transcript
  and the delta sent to the page, so a snapshot and the deltas can never
  disagree.
- **Run management.** A prompt either starts a run or is queued; claiming the
  run hands back a cancellable context; finishing it clears the state and starts
  whatever was queued. Abort marks the run as aborted so its failure is not
  reported.
- **Auth.** Every route requires the server token, compared in constant time,
  and the default binding is loopback, because the console can run shell
  commands.
- **Token persistence.** The token is reused from the state directory across
  restarts, which is what keeps an already-open page working when the process is
  restarted.
- **Opening another conversation** aborts the run in flight, waits for it to
  finish so its remaining events cannot land in the new one, swaps the session,
  and rebuilds the transcript from the agent's history. The page keeps the
  session id in the URL, so a reload reopens the same conversation.

## 7. Configuration and startup

Two parts share the work: the config package reads files and resolves XDG
directories; the entry point decides precedence and validates.

- Directory resolution follows the XDG specification, ignoring relative values
  and falling back to the usual home-relative defaults.
- File loading prefers an explicit path (which must exist), then TOML, then
  JSON. Unknown keys and a world-readable API key are reported as warnings
  rather than errors, because seeing a typo is worth more than refusing to
  start.
- **Precedence is applied once, generically**, per setting: a flag the user
  actually typed, then the environment, then the config file, then the default.
  Every resolved value records where it came from, which is what `--show-config`
  prints, with credentials masked. A setting whose zero value means "use the
  default" is resolved separately, because it cannot be told apart from an
  unset flag.
- Startup is a fixed sequence: parse and validate the flags; resolve and load
  configuration; answer the informational flags; read a piped prompt; build the
  provider, the tools and the system prompt (including context files); choose
  and install the session; then select the front end. Selecting between
  interactive and print by whether standard input and output are terminals is
  what makes a piped run behave like a one-shot run.

## 8. Concurrency model

- One run at a time, in both front ends. The run executes on its own goroutine
  and the interface never blocks on the model.
- Communication is one-way: the loop emits events, and the front end folds them
  on its own thread — the bubbletea update loop in the terminal, the server
  mutex in the browser.
- Shared mutable state is kept small and guarded: the terminal UI is
  single-threaded by construction, and the web server holds one mutex and never
  calls into the agent while a run is in flight.
- Cancellation travels through the run's context, so it reaches the provider
  call, the retry wait and a spawned shell alike; an abort is prompt rather than
  polled.

## 9. Testing strategy

The suite is layered, and the layers exist to keep the real loop under test:

- **End to end.** A scripted server speaks the provider protocol over a real
  HTTP connection, and the real loop runs against it with the real tools in a
  temporary directory. This covers a tool round trip, chained tools, a failure
  fed back to the model, allowlists, provider errors, print and JSON output,
  context-file discovery, session persistence and resume, path resolution,
  configuration precedence, `--show-config`, retries and the identity headers.
- **Unit tests per package**, each against the smallest real thing it can use: a
  fake server for the provider's classification and timeouts, temporary
  directories for the tools, real files for sessions, and injected waits so the
  retry policy is tested without sleeping.
- **Front ends against a fake agent.** Both interfaces run against a scripted
  fake of the same small interface the real agent satisfies, so folding events,
  aborting, queueing, opening another conversation and the auth checks are
  covered without a terminal or a socket.
- **Layout and presentation** are tested where they can break: the footer's
  segments and the transcript blocks a notification produces.

The same rules apply everywhere: no fixed temporary paths, no assumptions about
the host shell beyond what the shell tool documents, and non-ASCII text where a
count or a width depends on it. The suite runs natively on every platform the
release builds for, so a platform assumption is a failing test rather than a
report from a user: a permission bit Windows does not carry, a path separator, a
drive colon in a directory name, or a stand-in binary the host cannot execute.

## 10. Extension points, and what is deliberately absent

The seams a design change would use, without redesigning the core:

- A new provider implements the provider interface; runtime mutation of the
  model or the session id is optional through the small optional interfaces, so
  a provider that cannot do either is never asked to.
- A new tool declares its schema and its run and is added to the registry's name
  lists.
- A new front end implements the same small agent interface, consumes the same
  event stream, and takes its session controls from the same callbacks.

All three are constrained by `design/features/basic-functions`: the tool set is
fixed at seven, one provider protocol is packaged, and the four interfaces are
the supported ones. Anything beyond that — extensions, sub-agents, MCP, other
vendors — is out of scope until a design document says otherwise, not something
to be added as a local convenience.

A feature that needed several of these seams at once is
`design/features/context-management`: a context-window indicator and compaction
of older messages. How it is built is the separate item
`design/technical/context-compaction`.
