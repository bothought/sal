# Context compaction

This document is the technical description of the context window feature
specified in `design/features/context-management`: how the limit is known, how
the used figure is measured and estimated, where the threshold check sits in the
agent loop, how a compaction is represented in the session file, and how the two
front ends render it. What the feature *does*, from the user's side, is that
document; this one is how it is built.

## 1. Where each piece lives

The feature spans four packages, and the split follows the existing dependency
direction (`design/technical/architecture` §1):

```
internal/agent/     context accounting, the limit, the compaction policy and
                    the summarization request; emits context/compaction events
internal/provider/  reports usage (already does) and the model's context limit
internal/session/   stores compaction entries and rebuilds the active context
internal/tui/       footer segment, /context and /compact, notification block
internal/web/       same, over the existing snapshot/delta event stream
main.go             resolves the three settings and hands them to the agent
```

Nothing about the feature is front-end specific: the loop decides what the model
is sent and reports the figures; the front ends render them. This is why it
cannot be implemented in one interface.

## 2. The limit

### 2.1 Resolution

`internal/agent` owns the limit, because the threshold check is there. The
resolution order is the one the feature document states:

1. An explicit value from configuration (`--context-window` / `context_window`).
   `main` parses it (accepting a `k`/`m` suffix) and sets `Agent.ContextWindow`
   before the first run; nothing overrides it afterwards.
2. Model metadata from the endpoint, when the provider can supply it.
3. A built-in table, matched by model id prefix, in `internal/agent`.
4. The default of 128k.

The result is cached per model name, so a long run does not re-resolve it on
every turn. `/model` invalidates the cache entry for the new name, which is what
makes the limit follow the model.

`Provider` gains one optional method, reached by type assertion the way
`ModelSetter` already is:

```go
// ContextLimiter is implemented by providers that can report the model's
// context window. Providers that cannot are asked nothing.
type ContextLimiter interface{ ContextWindow(ctx context.Context) (int, bool) }
```

The OpenAI client implements it against `GET {base}/models` when the response
carries a limit, and reports `false` otherwise — including on any error, since a
missing limit is not a failure. The call is made at most once per model, lazily,
and only when configuration has not supplied a value.

The built-in table is a small map from model-id prefix to window size
(`gpt-4o`, `gpt-4.1`, `o1`, `o3`, `o4`, `claude-*` reachable through a gateway,
`llama3`, `qwen`, `deepseek`, …), deliberately conservative where a family
varies. It lives in one file so it is obvious what it is.

### 2.2 What the limit means

The limit is what the model accepts in one request. `Agent.ContextWindow()`
returns the resolved value plus where it came from, so `/context` can print the
source; the footer prints only the number.

## 3. The used figure

### 3.1 Measured

`Usage` already carries the provider's input and output counts. After a response,
`input + output` is the exact size of the context that was sent plus the answer,
so the agent records it as the last measured figure along with the message index
it was measured at.

### 3.2 Estimated

Everything appended after that point is estimated, and the estimate is added to
the measured base. Estimation lives in one function in `internal/agent`:

```go
// EstimateTokens approximates the token count of s.
func EstimateTokens(s string) int
```

It counts ASCII runes at roughly four characters per token and non-ASCII runes at
roughly one token each, so CJK text is not under-counted by a factor of four.
The message overhead (role, separators, tool-call framing) is added per message
as a small constant. The result is an estimate by construction and is never
presented as measured.

The agent keeps:

- `measured int` and `measuredAt int` (message index), and
- `measuredOK bool`,

and computes `UsedTokens()` as `measured` plus the estimate of everything after
`measuredAt`, or the estimate of the whole context when nothing has been measured
yet. The system prompt and the tool definitions are part of the base: they are
estimated once per model/tool-set and cached, since they do not change within a
session.

`UsedTokens()` returns `(used int, exact bool)`. `exact` is what the interfaces
render as the absence or presence of `~`.

### 3.3 Invalidation

`measured` is dropped when the context changes shape rather than grows: `/new`,
a session switch, a resume, a compaction, and a model change all reset it, so a
stale exact figure is never shown for a different context. The next response
re-establishes it.

## 4. The threshold check

The check belongs to `Agent.RunPrompt`, before the first request of a run and
before each subsequent request in the same run — the two places where the context
is about to be sent. Concretely, at the top of the turn loop:

```go
if a.shouldCompact(used) { a.compact(ctx, emit, compactAuto, "") }
```

`shouldCompact` is false when automatic compaction is off, when the run has
already compacted, when the loop has disabled further attempts (§4.2), or when
`used` is below `CompactAt` percent of the limit.

### 4.1 The summarization request

Compaction builds a separate request from the messages being summarized, plus an
instruction message describing what the summary must contain (the feature
document's list), plus any user guidance from `/compact <instructions>`. It
carries **no tool definitions**, so the answer can only be text.

It is sent through the existing `Provider.Stream`, so the retry policy applies
unchanged, and the answer is not appended to the conversation. The call is
wrapped in `streamWithRetry` with a distinct emit filter so a retry of the
summary does not look like a retry of the user's turn.

### 4.2 Not looping

The agent holds `compactFailed bool` and `autoDisabled bool` for the session.
Compaction runs at most once per check, the check runs at most once per request,
and if the context is still at or above the threshold afterwards, `autoDisabled`
is set and one system event says automatic compaction is off for the session.
That is what makes "compaction never loops" true by construction rather than by
counting.

### 4.3 Cut point and target

The messages are split at a boundary the conversation allows: walking backwards
from the end, the agent keeps `KeepTurns` (3) user turns, and the cut lands
before the first message of the oldest kept turn, never between an assistant
message with tool calls and its results. If the kept tail alone exceeds the
target (50% of the limit), tool-result contents inside the tail are shortened
oldest-first to an `output omitted` marker until the target is met; the count of
shortened results is reported.

### 4.4 Chunking

If the messages being summarized are themselves larger than the limit, they are
summarized oldest-first in chunks that fit, each chunk's request including the
running summary so far, and the final answer is the summary. This path exists
because the context being compacted is at the limit by definition.

## 5. The active context versus the history

`Agent.History` stays the complete conversation, as it is today, because sessions
persist through `OnMessage` and the transcript is built from it. Compaction does
not delete from `History`; it records that a prefix of it has been replaced by a
summary:

```go
// Compacted is the active summary of the conversation prefix it replaces.
type Compacted struct {
    Summary   string
    Through   int    // messages 0..Through are represented by Summary
    Messages  int    // how many that is
    CreatedAt string
    Saved     int    // tokens before minus after, for the notification
    Shortened int    // tool results shortened to meet the target
}
```

`Agent.Compaction *Compacted` is what `requestMessages()` consults: it emits the
system prompt, then the summary as one labelled user message, then
`History[Through+1:]`. This is the single place where what the model sees differs
from the history, and it keeps the message sequence valid because the summary
occupies a user role.

## 6. Events

Two event types are added to the agent's stream, and every front end folds them
the way it folds the existing ones:

- `context` — carries `used`, `limit`, `exact`, and the source of the limit.
  Emitted with each `usage` event (so the footer updates when a measurement
  arrives) and once at the start of a run.
- `compaction` — carries the summary's message count, the token counts before
  and after, whether it was automatic, and how many tool results were shortened.

`Event` gains `Context *ContextInfo` and `Compaction *CompactionInfo` fields,
with the JSON tags the other fields use, so `--mode json` reports them without
any further work.

## 7. Sessions

`internal/session` gains one entry type:

```go
// Entry.Type == "compaction"
type Compacted struct {
    Summary   string `json:"summary"`
    Through   string `json:"through"`   // id of the last message it replaces
    Messages  int    `json:"messages"`
    Shortened int    `json:"shortened,omitempty"`
}
```

The entry is appended when a compaction happens, and every message stays in the
file. `Load` keeps the compaction in `Session.Compaction` (the last one wins) and
still appends every message to `Messages`, so the file remains a complete record.

On resume, `main` hands the compaction to the agent, which reconstructs the
active context exactly as §5 describes: the summary plus the messages after
`Through`. `Through` is a message id, not an index, so it survives any change in
how messages are counted.

A session is marked as compacted in `session.Info` (one boolean), which is what
the switchers show.

## 8. Front ends

### 8.1 Terminal UI

- The footer gains the context segment between the status and the cumulative
  counters: `ctx 12.4k/128k 10%`, with `~` when the figure is estimated. The
  colour follows the severity table in the feature document, reusing the existing
  footer style and the warning/error colours already in use.
- `handleEvent` folds `context` into the model's footer state and `compaction`
  into a system block:
  `context compacted — 18 messages summarized · 105k → 24k tokens · the last 3 turns kept`.
- `/context` renders the breakdown from the same state; `/context summary` prints
  `Agent.Compaction.Summary`.
- `/compact [instructions]` calls into the agent through the existing `Agent`
  interface, gaining two methods (`Compact(ctx, instructions) error` and
  `ContextInfo() (used, limit int, exact bool)`); when a run is in flight the
  command is queued like a prompt.

### 8.2 Web console

- `outEvent` gains `ctxUsed`, `ctxLimit`, `ctxExact`, and a `compaction` event
  type; `statusLocked` fills the context fields from the agent, so the footer
  segment and a reloading client get them from the same snapshot.
- `index.html` renders the segment in the footer, adds the two commands to the
  command switch, and appends the compaction notification block.
- `/compact` posts to `/api/compact`; the handler mirrors the existing
  `handleNew`/`handleModel` shape and refuses while a run is in flight by
  queueing, exactly as the TUI does.

## 9. Configuration

`main` gains three flags and `internal/config.File` three keys, resolved with the
existing `resolveOptions` helpers so they appear in `--show-config` with their
source:

| Flag | Key | Default | Note |
|---|---|---|---|
| `--context-window` | `context_window` | 0 | 0 = resolve as §2.1; accepts `128k`/`1m` |
| `--auto-compact` / `--no-auto-compact` | `auto_compact` | on | pointer in `File`, like `no_session` |
| `--compact-at` | `compact_at` | 80 | 1..100, validated by `validateResolved` |

`KeepTurns` and the 50% target are constants in `internal/agent`, not settings.

## 10. Testing

The suite follows the existing layering:

- `internal/agent` unit tests: limit resolution and its precedence, the
  estimator (including CJK text), `UsedTokens` exactness before and after a
  measurement, the threshold check, the cut point across a tool-call boundary,
  the target-size shortening, the no-loop behaviour, and a failed summarization
  leaving the context unchanged. The provider is a scripted fake, as in the
  existing retry tests.
- `internal/session`: a compaction entry round-trips, and `Load` reconstructs
  the summary and the messages after it.
- End to end (`e2e_test.go`): a scripted SSE server whose reported usage crosses
  the threshold, driving a real run through an automatic compaction and asserting
  the request that follows carries the summary instead of the compacted messages.
- `internal/tui` and `internal/web`: the footer segment, the notification block,
  the two commands, and `/compact` while a run is in flight.

## 11. What this deliberately does not do

The non-goals of the feature document apply here too: no tokenizer dependency, no
per-vendor token counting, no cost accounting, no undo, and no rewriting of the
session file. The implementation keeps the feature's own boundary — a limit, a
figure, and a summary — and nothing more.
