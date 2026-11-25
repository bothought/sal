# Context compaction

The technical design of `design/features/context-management`: where the context
figures and compaction live, how the parts share one policy, and which invariants
keep them honest.

## 1. Shape

The feature adds responsibilities to existing parts:

- **The agent owns the policy.** It decides what the model is sent, whether the
  window is full enough to compact, what a compaction may cover, and produces the
  summary. Every interface therefore behaves the same way.
- **The provider answers one question.** It reports the selected model's window
  when the endpoint publishes one, and says so when it does not. That is not an
  error.
- **The session stores the record.** A compaction is an entry in the conversation
  log, and the log stays complete.
- **The console renders.** It folds two events into a footer segment, a
  notification block and two commands. It computes no figure and decides nothing.

Print and JSON runs compact through the same loop; they differ only in how they
report it.

## 2. The context window

Three answers, each remembered for the conversation:

1. **The limit**, resolved per model: explicit configuration, the endpoint's
   metadata, a built-in table keyed by model family, then a conservative default.
   The agent keeps the value and its source. The console can ask for the resolved
   value without causing a provider request.
2. **Used**, the size of the next request, with a flag saying whether it is
   measured. The provider's reported input plus output covers the whole sent
   context, so it is exact until something is appended; everything after that is
   estimated on top of the measured base. The system prompt and tool definitions
   are part of the base and are accounted once.
3. **The threshold**, a percentage of the limit. Severity bands use the same
   setting, so “critical” means compaction is due or just happened.

Estimation is one rule for text plus a small per-message overhead, chosen so
dense scripts are not under-counted. It is not a tokenizer; the interface says
the figure is approximate.

The measured base is only valid for the context it was measured in. A fresh
conversation, a session switch, a resume, a compaction, or a model change drops
it. The next response re-establishes it.

## 3. Compaction

Compaction is a policy decision plus one provider request. It never mutates the
stored conversation.

- **Output.** The oldest part of the active conversation is replaced, as far as
  the model is concerned, by one summary message in the user role. The
  summarization request is separate, carries no tool definitions, and is not
  appended to the conversation.
- **Kept.** The system prompt, tool definitions, and a fixed number of recent
  user turns with their assistant messages and tool results. The cut is always a
  boundary where the conversation is consistent: a tool call stays with its
  results. The summary in the user role makes the result valid by construction.
- **Target.** Compaction aims to leave the context at a fraction of the window,
  not exactly at the threshold. If the kept tail alone exceeds that target, its
  oldest tool results are replaced by a marker, and the fact is reported.
- **Repeated compaction.** A second compaction starts from the previous summary
  and subsumes it, so the recorded count is cumulative.
- **Oversized input.** If the messages to summarize exceed the window, they are
  summarized oldest-first in parts, each carrying the running summary. This path
  exists because the context being compacted is at the limit by definition.
- **Failure.** A failed summarization leaves the context exactly as it was. A
  manual failure is reported; an automatic one proceeds with the uncompacted
  context, because the next request's outcome is what the user should see.

## 4. Invariants

- **History is never shortened.** The agent keeps the full conversation, and so
  does the store. Compaction records that a prefix is represented by a summary;
  only the view used to build a request changes.
- **The view is built in one place.** The system prompt, summary and retained
  tail are assembled once. The figure, the request and a rebuilt transcript all
  read the same view.
- **The check runs once per request and cannot loop.** If compaction fires and
  the context is still full, automatic compaction stops for the session and says
  so, unless there was nothing to summarize.
- **Compaction only happens between turns.** It never runs during a streamed
  answer or with a tool call outstanding.
- **Every attempt is reported.** The interface learns about every compaction,
  including ones that did nothing, so a shorter session has an explanation.

## 5. Data flows

- **Agent to console.** The context figure, its breakdown and the active summary
  travel on the event stream with every compaction. The console does not ask the
  agent for a figure while a turn is running; the run goroutine owns the agent
  then. A snapshot taken while idle is the only other source.
- **Agent to session.** A compaction reaches the session through a persistence
  callback, stored with the summary, the message it replaces, the message count,
  and the shortened tool results. Bookkeeping is by message identity, not
  position.
- **Session to agent.** On resume, the stored compaction is handed to the agent
  before the conversation is used, and the active context is reconstructed from
  it. A rebuilt transcript asks for the same view.
- **Main to agent.** The three settings resolve with normal precedence and are
  handed over before the first run; the agent never parses configuration.

## 6. Console

The console keeps its own cached copy of the figures so a redraw never reaches
into the agent. It renders:

- a footer segment with the figure and severity;
- a transcript block for each compaction;
- the summary through `/context summary`;
- `/context` and `/compact`, thin wrappers over the cached figures and one call
  into the agent. Compaction is queued like a prompt when a turn is in flight.

A page that connects mid-run receives the cached figures in the snapshot.

## 7. Non-goals

No tokenizer dependency, no per-vendor token counting, no cost accounting, no
undo, and no rewriting of the session file. There is one limit, one figure, one
summary, and the smallest number of places that can be wrong about them.
