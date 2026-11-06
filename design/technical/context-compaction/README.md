# Context compaction

This document is the technical description of the context window feature
specified in `design/features/context-management`: where the context figures and
the compaction live, how the program is split so that every mode behaves the
same, what flows between the parts, and which invariants the structure
guarantees.

It describes the architecture, not the implementation. What the feature does is
the feature document; how each part achieves it is in the code and its doc
comments.

## 1. Shape

The feature is not a new component but a set of responsibilities added to parts
that already exist:

- **The agent owns the policy.** Deciding what the model is sent, whether the
  window is full enough to compact, what a compaction is allowed to cover, and
  producing the summary are all agent responsibilities, so the four interfaces
  behave identically and none of them can get it wrong on its own.
- **The provider answers one question.** When the endpoint publishes the
  selected model's window, it reports it; when it does not, it says so, and that
  is not an error.
- **The session stores the record.** A compaction is one more entry in the
  conversation log, and the log stays complete.
- **The front ends render.** Both interfaces fold the same two new events into a
  footer segment, a notification block and two commands. Neither computes a
  figure, and neither decides when to compact.

This split is the reason the feature works in print and JSON runs too: those
modes have no interface, but the loop that compacts is theirs as well.

## 2. The context window

Three things are decided by the agent, in this order, and each answer is
remembered for the rest of the conversation:

1. **The limit**, resolved per model, first source wins: explicit
   configuration, then the endpoint's model metadata, then a small built-in
   table keyed by model-id family, then a conservative default. The agent keeps
   the value and where it came from, because the source is what makes an assumed
   figure honest in `/context`; the footer shows only the number. An interface
   must be able to ask for the resolved value without causing a provider
   request, since drawing a footer is not a reason to touch the network.
2. **The size of the next request** ("used"), as a figure plus an indication of
   whether it is measured or estimated. The provider's reported input plus
   output covers the whole context it was sent, so it is exact until something is
   appended; everything after that point is estimated, and the estimate rides on
   top of the measured base rather than replacing it. The system prompt and the
   tool definitions are part of the base and do not change within a
   conversation, so they are accounted once.
3. **The threshold**, a percentage of the limit. The severity bands the
   interfaces show are expressed in terms of that same setting, so "critical"
   always means "compaction is due or has just happened" rather than a second
   set of magic numbers.

Estimation is one rule applied to text and one small per-message overhead,
chosen so that text in a dense script is not reported at a fraction of its real
size. It is deliberately not a tokenizer: the figure is approximate and the
interfaces say so.

The measured base is only valid for the context it was measured in. Anything
that changes the context's *shape* rather than extending it — a fresh
conversation, opening another directory's conversation, a resume, a compaction,
a model change — drops
it, so a stale exact figure is never shown for a different context. The next
response re-establishes it.

## 3. Compaction

Compaction is a policy decision plus one provider request; it is never a
mutation of the conversation.

**What it produces.** The oldest part of the active conversation is replaced, as
far as the model is concerned, by one summary message in the user role. The
summary is produced by the same model, in a request that is not part of the
conversation, carries no tool definitions, and whose answer is never appended to
the conversation. It is labelled as a record of earlier conversation, because
the model must not mistake it for an instruction, and the instruction that
produces it says explicitly that a summary records and does not plan.

**What is kept.** The system prompt, the tool definitions, and a fixed number of
the most recent user turns with their assistant messages and tool results. The
cut is always a message boundary at which the conversation is consistent: an
assistant message that requested tools stays with the results that answered it,
and the summary never stands where a tool call or a tool result would be
orphaned. The summary taking the user role is what makes the resulting sequence
valid by construction.

**The target.** Compaction aims to leave the context at a fraction of the window
rather than exactly at the threshold, so the session has room for several more
turns. When the kept tail alone is larger than that target, the oldest tool
results inside it are replaced by an "output omitted" marker, and the fact is
reported. That is the one exception to keeping the tail verbatim.

**Repeated compaction.** A second compaction starts from the previous summary as
its running state instead of re-summarizing the messages it already represents,
so the new summary subsumes the old one and the recorded count is cumulative.

**Oversized input.** If the messages being summarized are themselves larger than
the window, they are summarized oldest-first in parts, each part carrying the
running summary, and the last answer is the summary. This path exists because
the context being compacted is at the limit by definition, so it is the path
that must not fail.

**Failure.** A failed summarization leaves the context exactly as it was. A
manual compaction reports the error; an automatic one stays out of the run's
way, because the point of compacting is to let the next request happen, and the
next request's own outcome is what the user should see.

## 4. Invariants

- **The conversation the agent holds is never shortened.** History stays
  complete, and so does the session file. Compaction records that a prefix of
  the history is represented by a summary; what changes is the view the request
  is built from, not the record.
- **That view is built in one place.** Everything the model is sent — the
  system prompt, the summary, the retained tail — is assembled once, and the
  figure, the request and any transcript a front end rebuilds all read the same
  view. Nothing else in the program has a second opinion about what the model
  sees.
- **Compaction is checked once per request and cannot loop.** The check sits
  immediately before each request in the loop. If it fires, it fires once: when
  the context is still full afterwards — because the fixed part is large, or
  because there is nothing left to summarize — the agent stops trying for the
  rest of the session, and says so unless there was nothing to summarize.
  Summarizing repeatedly for no gain is worse than sending and failing once.
- **Compaction only happens between turns.** It never runs during a streamed
  answer or while a tool call is outstanding, so the cut is always a boundary
  the conversation allows.
- **Compaction is reported, not performed silently.** Every attempt reaches the
  interfaces through the event stream, including the ones that did nothing, so
  "why did my session get shorter" always has an answer.

## 5. Data flows

- **Agent → interfaces.** The context figure, its breakdown and the active
  summary travel on the event stream, together with every compaction. The
  interfaces never ask the agent for a figure while a turn is running, because
  the run goroutine owns the agent then; a snapshot taken while idle is the only
  other source. This is what keeps the terminal footer, the web footer and a
  JSON consumer reading the same numbers.
- **Agent → session.** A compaction reaches the session through a persistence
  callback, in the same way messages do, and is stored as its own entry: the
  summary text, the message it replaces, how many messages that is, and how many
  tool results were shortened. The bookkeeping is by message identity rather than
  position, so it survives any change in how messages are counted.
- **Session → agent.** On a resume, the stored compaction is handed to the
  agent before the conversation is used, and the active context is reconstructed
  from it. A front end rebuilding a transcript from a resumed conversation asks
  the agent for the same view, so it shows the summary and the tail rather than
  replaying messages the model is not sent.
- **Main → agent.** The three settings are resolved with the same precedence as
  every other setting, validated once, and handed over before the first run, so
  the agent never parses configuration.

## 6. Front ends

Both interfaces are folds over the same events, and both keep their own cached
copy of the figures so a redraw never reaches into the agent:

- A footer segment showing the figure and its severity, in the same place
  relative to the existing segments in each interface.
- A transcript block for each compaction, and the summary text available through
  the context command, so the loss is auditable.
- The two commands. Both are thin wrappers: the breakdown comes from the cached
  figures, and compaction is one call into the agent, queued like a prompt when
  a turn is in flight because compaction needs a stable conversation.

The web console additionally caches the figures in the server's snapshot, which
is what lets a page that connects mid-run get a consistent footer without asking
the agent. A resumed conversation reports that it was compacted, so the gap
between a file's message count and the active context is explained.

## 7. What this deliberately does not do

The feature's non-goals hold here too: no tokenizer dependency, no per-vendor
token counting, no cost accounting, no undo, and no rewriting of the session
file. The architecture is one limit, one figure, one summary, and the smallest
number of places that can be wrong about them.
