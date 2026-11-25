# Context management

This item covers the context-window indicator and compaction: how much room is
left, what happens when it runs out, and how a long session keeps working. How it
is built is `design/technical/context-compaction`.

## 1. Vocabulary

- **Context window** — the maximum tokens the model accepts in one request,
  including the system prompt, tool definitions, and every message.
- **Context** — what the *next* request will send: the system prompt, the tool
  definitions, and the active conversation.
- **Used** — the current size of the context in tokens.
- **Limit** — the active model's context window.
- **Compaction** — replacing the oldest part of the active conversation with a
  summary so `used` drops while the thread of work survives.
- **Active context** — the messages the model sees; after a compaction it is
  smaller than the stored log.

## 2. The context indicator

### 2.1 Display

The web console footer carries a single segment between the status and the
cumulative token counters:

```
gpt-4o-mini · ready · ctx ~12.4k/128k 10% · ↑120k ↓8.2k · 3 turns
```

`used` is the size of the *next* request; `↑`/`↓` remain the cumulative spend. A
`~` marks an estimated figure. Before the first response the whole figure is
estimated. After a response the provider's reported input and output token counts
make it exact until something is appended; anything added after that is
estimated, so the figure becomes approximate again.

Print and JSON runs have no footer. JSON mode reports the same figures on its
event stream; print mode stays quiet about them except when compaction happens.
Compaction works in all three interfaces.

### 2.2 Severity

| Occupancy | State | Presentation |
|---|---|---|
| below 70% | normal | default colour |
| 70% to the auto-compaction threshold | warning | warning colour |
| at or above the threshold | critical | error colour; `compacting…` while an automatic compaction runs |
| above 100% | over | error colour; `over by N` |

The bands use the configured auto-compaction threshold, so “critical” always
means compaction is due or just happened. They apply whether automatic compaction
is enabled or not.

### 2.3 Where the limit comes from

1. `--context-window <tokens>` or `context_window` in the config file.
2. The endpoint's model metadata, when it publishes a context limit.
3. A small built-in table of common model families, matched by model-id prefix.
4. A conservative default of 128k.

The limit is always known. `/context` names the source; the footer shows only the
figure. Resolution happens before the first request of a run; drawing the footer
does not contact the endpoint. `/model` discards the resolved value and asks
again, because the window belongs to the model.

### 2.4 Measuring “used”

The figure covers:

- the system prompt, including context files;
- the definitions of the enabled tools;
- every message in the active context, including tool results;
- the active summary after a compaction.

Estimation is approximate and must remain reasonable for non-ASCII text. The
exact figure arrives with the next response.

`/clear` does not change the figure. `/new` resets it to the system prompt and
tool definitions. Switching or resuming a session recomputes it from the restored
context.

### 2.5 `/context`

`/context` prints the breakdown and where each number came from:

```
context
  model         gpt-4o-mini
  window        128k (from the built-in table)
  used          12,431 tokens (10%)
    system       1,204
    tools          812
    messages    10,415
      summary     1,832 (18 earlier messages)
      recent      8,583 (6 messages, 3 turns)
  headroom      115,569
  auto-compact  on, at 80% (102k)
  last request  12,398 input + 214 output (measured)
```

The per-category numbers are estimates; the `used` line is the figure the footer
shows. `/context summary` prints the text of the active summary.

## 3. Compaction

### 3.1 What it does

Compaction takes the oldest part of the active conversation, asks the model to
summarize it, and replaces that part in the active context with:

```
[summary of the conversation so far]
<the summary>
[the most recent turns follow]
```

The summary is inserted as a user message, so the result is always a valid
conversation: a tool call never loses its result and a result never loses its
call. The system prompt and tool definitions are never compacted.

The summarization request carries no tool definitions, so the model can only
answer with text. It uses the normal retry policy but does not produce `retrying`
notices of its own. It is not appended to the conversation and is billed like any
other request.

### 3.2 What is kept

- The system prompt and context files.
- The enabled tool definitions.
- The most recent three user turns, with their assistant messages and tool
  results.
- A tool result in that tail may still be replaced by an
  `[output omitted to fit the context window]` marker when the target cannot
  otherwise be met; the compaction notification says when that happened.

Compaction never splits a unit: an assistant message that requested tools stays
with the results that answered it. Everything older than the kept tail survives
only in the summary.

### 3.3 What the summary contains

The summarization prompt asks for a compact, factual record of:

- what the user asked for, including constraints and preferences;
- decisions and their reasons;
- files read, created, and modified, with paths;
- commands run and what they showed, including failures and fixes;
- errors encountered and how they were resolved;
- the current state of the work and what remains;
- anything the user asked to remember or avoid.

The summary is labelled as a record, not instructions, and must not claim work
was completed when it was not. Repeated compaction subsumes the previous summary
into a new one.

### 3.4 Automatic compaction

Automatic compaction is on by default. Before each request in a run, the agent
checks whether the projected context has reached the threshold:

- threshold: **80%** of the window (`--compact-at <percent>` / `compact_at`);
- the check runs once per request and never loops;
- compaction never runs during a streamed answer or with a tool call
  outstanding;
- the target is **at most 50%** of the window, leaving room for several turns;
- if the kept tail alone is larger than the target, its oldest tool results are
  shortened until it fits;
- if compaction cannot bring the context below the threshold, automatic
  compaction is disabled for the rest of the session and one warning says so;
  when there is nothing left to summarize, it is disabled without a warning;
- `--no-auto-compact` / `auto_compact = false` turns the trigger off. The
  indicator still warns and an oversized request fails with the provider's error;
  manual `/compact` still works.

Automatic compaction applies in print and JSON runs as well as the console.

### 3.5 `/compact`

```
/compact [instructions]
```

- Compacts immediately when the console is idle.
- While a turn is in flight, the command is queued after the current turn; the
  queue holds the command, so it is never sent as a prompt.
- The optional text is passed to the summarization request as what to preserve.
- With too little conversation to summarize, it reports that there is nothing to
  compact and does nothing.
- It is a console command only; print and JSON runs compact automatically.

### 3.6 What the user sees

- Before: the footer shows the figure in the error colour at the threshold.
- During an automatic compaction: status `compacting…`; a prompt submitted then
  is queued.
- After: a transcript block records the result:

  ```
  context compacted
    18 messages summarized · 105k → 24k tokens · the last 3 turns kept
  ```

  The block also says when tool results were shortened, when automatic
  compaction disabled itself, or why a compaction failed. The summary text is
  available with `/context summary`.
- A manual compaction with nothing to summarize, or one that fails, adds a block
  saying so.
- The visible transcript is not rewritten: the summarized messages stay on
  screen. The transcript is the user's record; compaction changes only what the
  model is sent.
- In JSON mode, compaction emits a `compaction` event with the counts, token
  figures, shortening and automatic/requested flag.
- In print mode, one notice goes to stderr.

### 3.7 Failure and interruption

- A summarization failure leaves the active context unchanged and is reported.
  An automatic failure does not block the run; the request proceeds with the
  uncompacted context and its own outcome is what the user sees.
- If the summarization input is larger than the window, the conversation is
  summarized in parts, oldest first, each part carrying the running summary.
- Aborting (`esc`) during an automatic compaction cancels it and leaves the
  context unchanged; the block says the compaction was cancelled.
- Opening another conversation or `/new` drops a queued compaction with the
  conversation it belonged to.
- Compaction never runs while a tool call is outstanding or mid-answer.

## 4. Configuration

| Setting | Flag | Config key | Default |
|---|---|---|---|
| Context window override | `--context-window <tokens>` | `context_window` | 0 = discover |
| Automatic compaction | `--auto-compact` / `--no-auto-compact` | `auto_compact` | on |
| Threshold | `--compact-at <percent>` | `compact_at` | 80 |
| Kept tail | — | — | 3 turns |
| Summary target | — | — | 50% of the window |

All resolve with **flag > environment > config file > default** and appear in
`--show-config` with their source. `context_window` accepts `128k`, `1m`, or a
plain number. `context_window` must not be negative and `compact_at` must be
between 1 and 100. No new environment variables are introduced.

With `--no-session`, compaction still works but is not recorded, so it is lost
when the process exits.

## 5. Interaction with existing behaviour

### 5.1 Sessions

The stored conversation keeps every message. A compaction is recorded separately:
the summary, the message it replaces, the number of messages covered, and the
tool results it shortened. On resume the active context is reconstructed from the
summary onwards. The rebuilt transcript shows the summary and the messages after
it, not the summarized prefix.

### 5.2 `/clear` and `/new`

`/clear` empties the visible transcript and touches neither the conversation nor
the indicator. `/new` starts a fresh conversation: the indicator resets and no
summary carries over. Opening another directory's conversation recomputes the
limit and the used figure.

### 5.3 Model switching

`/model <name>` changes the limit and therefore the percentage and threshold
immediately. If the new window is smaller than the current context, the next
request is preceded by an automatic compaction when enabled; with it disabled,
the request is attempted and the provider's error is reported.

### 5.4 Context versus the stored log

After a compaction the active context and the stored log differ by design. The
footer and `/context` report the active context, never the log. The console says
when this is the case rather than hiding it.

### 5.5 Security and cost

The summary is model output and has the same trust boundary as assistant text;
prompt-injected content can survive into it. It is labelled as a summary, and
`/context summary` shows the user the same text. Compaction is lossy: details the
summary omits leave the model's context, though they remain stored. It sends part
of the conversation to the provider a second time, at the moment the context is
largest; one expensive request is the intended trade for continuing the session.

## 6. Non-goals

- Exact tokenization or per-vendor token counting.
- A vendor matrix of model limits.
- Cross-session memory, retrieval, or vector storage.
- Selective pinning, editing, or re-fetching of messages.
- Undo of a compaction within the active context.
- Cost accounting.
- Automatic switching to a larger-window model.
- Rewriting the visible transcript when compacting.
