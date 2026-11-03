package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNothingToCompact is returned when the conversation is too short to
// summarize: only the turns that compaction keeps verbatim are present.
var ErrNothingToCompact = errors.New("nothing to compact yet: the conversation is still short")

// Compact replaces the oldest part of the active conversation with a summary.
// It is used by the /compact command (automatic false) and by the threshold
// check in RunPrompt (automatic true). The context is left untouched when the
// summarization request fails.
func (a *Agent) Compact(ctx context.Context, instructions string, automatic bool) (CompactionInfo, error) {
	limit, _ := a.resolveLimit(ctx)
	before, _ := a.UsedTokens()

	keep := a.compactionKeepCount()
	if keep <= 0 {
		return CompactionInfo{}, ErrNothingToCompact
	}

	// The summary must fit, so the target is a fraction of the window. Chunking
	// (below) handles the case where the messages being summarized are larger
	// than that.
	target := limit * compactTargetPercent / 100

	// A repeated compaction subsumes the previous summary instead of
	// re-summarizing the messages it already represents: the earlier summary is
	// the running state the new one starts from.
	from, prior, represented := 0, "", 0
	if a.compaction != nil {
		from = a.compaction.Through + 1
		prior = a.compaction.Summary
		represented = a.compaction.Messages
	}
	cut := keep

	summary, err := a.summarizeFrom(ctx, from, cut, prior, instructions, target)
	if err != nil {
		return CompactionInfo{Before: before, Automatic: automatic, Error: err.Error()}, err
	}

	shortened := 0
	info := CompactionInfo{
		Messages:  represented + cut - from,
		Before:    before,
		KeptTurns: a.countTurns(cut, len(a.History)),
		Automatic: automatic,
	}
	a.compaction = &Compacted{
		Summary:  summary,
		Through:  cut - 1,
		Messages: info.Messages,
	}
	a.shortened = nil
	a.measuredOK = false

	// If the kept tail alone is larger than the target, shorten its tool results
	// oldest-first until it fits.
	if after, _ := a.UsedTokens(); after > target {
		// Only the kept tail is in the context, so that is what can be shortened.
		for i := cut; i < len(a.History); i++ {
			if after <= target {
				break
			}
			if a.History[i].Role != RoleTool || strings.TrimSpace(a.History[i].Content) == "" {
				continue
			}
			before := EstimateTokens(a.History[i].Content)
			a.shortened = append(a.shortened, i)
			after -= before - EstimateTokens(omittedMarker)
			shortened++
		}
		info.Shortened = shortened
		a.compaction.Shortened = shortened
		a.compaction.ShortenedIndexes = append([]int(nil), a.shortened...)
	}

	after, _ := a.UsedTokens()
	info.After = after
	if info.Messages == 0 {
		return info, ErrNothingToCompact
	}
	if a.OnCompaction != nil {
		a.OnCompaction(*a.compaction)
	}
	return info, nil
}

// countTurns counts the user turns in History[from:to], which is what the
// compaction notification reports as kept.
func (a *Agent) countTurns(from, to int) int {
	turns := 0
	for i := from; i < to; i++ {
		if a.History[i].Role == RoleUser {
			turns++
		}
	}
	return turns
}

// compactionKeepCount is how many history messages are kept verbatim, or 0 when
// there is nothing to summarize.
func (a *Agent) compactionKeepCount() int {
	keep := a.keepTurns()
	start := 0
	if a.compaction != nil {
		start = a.compaction.Through + 1
		if start > len(a.History) {
			start = len(a.History)
		}
	}
	// Walk backwards to the user message that opens the oldest kept turn, so the
	// kept part always starts at a user turn and a turn's tool calls stay with
	// their results.
	turns := 0
	cut := 0
	for i := len(a.History) - 1; i >= start; i-- {
		if a.History[i].Role != RoleUser {
			continue
		}
		turns++
		if turns == keep {
			cut = i
			break
		}
	}
	if cut <= start {
		return 0
	}
	// Never cut in the middle of a tool exchange: the message at the cut must
	// not be a tool result whose call is being summarized.
	for cut < len(a.History) && a.History[cut].Role == RoleTool {
		cut++
	}
	return cut
}

// summarizeFrom asks the model for a summary of History[from:to], chunking when
// that slice is too large to send in one request. prior is a summary that
// already covers everything before from (a repeated compaction), and is folded
// into the answer rather than summarized again.
func (a *Agent) summarizeFrom(ctx context.Context, from, to int, prior, instructions string, target int) (string, error) {
	if a.Provider == nil {
		return "", fmt.Errorf("no model provider configured")
	}
	// Chunk size: keep each request comfortably inside the window, leaving room
	// for the instruction and the answer.
	budget := target
	if budget <= 0 {
		budget = DefaultContextWindow / 2
	}
	instructionTokens := EstimateTokens(summaryInstruction(instructions))

	prior = strings.TrimSpace(prior)
	chunkStart := from
	for chunkStart < to {
		chunkEnd := a.fitChunk(chunkStart, to, prior, instructionTokens, budget)
		chunkEnd = a.chunkBoundary(chunkStart, chunkEnd, to)
		if chunkEnd == chunkStart {
			// A single exchange larger than the budget: send it whole, since a
			// cut inside it would leave a tool call without its result.
			chunkEnd = a.nextBoundary(chunkStart, to)
		}
		msg := a.summaryRequest(chunkStart, chunkEnd, prior, instructions)
		answer, err := a.streamSummary(ctx, msg)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(answer) == "" {
			return "", fmt.Errorf("the model returned an empty summary")
		}
		prior = strings.TrimSpace(answer)
		chunkStart = chunkEnd
	}
	if prior == "" {
		return "", ErrNothingToCompact
	}
	return prior, nil
}

// fitChunk returns the end index of the largest prefix of [start,end) that fits
// in budget together with the prior summary and the instruction.
func (a *Agent) fitChunk(start, end int, prior string, instructionTokens, budget int) int {
	used := EstimateTokens(prior) + instructionTokens + perMessageOverhead
	i := start
	for ; i < end; i++ {
		n := estimateMessage(a.History[i])
		if i > start && used+n > budget {
			break
		}
		used += n
	}
	return i
}

// chunkBoundary moves a provisional chunk end back to a point the conversation
// allows: a chunk never starts with a tool result whose call is in an earlier
// chunk, and never ends between an assistant message and the results that
// answer it. When start is already on a boundary, the end is moved forward
// instead (nextBoundary), so progress is always possible.
func (a *Agent) chunkBoundary(start, end, limit int) int {
	if end >= limit {
		return limit
	}
	// Walk back while the end would split a request from its results.
	for end > start && end < limit {
		prev := end - 1
		if a.hasToolCalls(prev) || a.isToolResult(end) {
			end--
			continue
		}
		break
	}
	if end > start {
		return end
	}
	return a.nextBoundary(start, limit)
}

// nextBoundary returns the first index after start at which a chunk may end:
// the end of the tool exchange that starts at start, or start+1 when start
// begins no exchange.
func (a *Agent) nextBoundary(start, limit int) int {
	i := start
	if a.hasToolCalls(i) {
		// The call and every result that answers it belong together.
		for i++; i < limit && a.isToolResult(i); i++ {
		}
		return i
	}
	if i < limit {
		return i + 1
	}
	return limit
}

func (a *Agent) hasToolCalls(i int) bool {
	return i >= 0 && i < len(a.History) && len(a.History[i].ToolCalls) > 0
}

func (a *Agent) isToolResult(i int) bool {
	return i >= 0 && i < len(a.History) && a.History[i].Role == RoleTool
}

// summaryRequest builds the message slice sent to the model for one chunk.
func (a *Agent) summaryRequest(from, to int, prior, instructions string) []Message {
	msgs := make([]Message, 0, to-from+1)
	if strings.TrimSpace(prior) != "" {
		msgs = append(msgs, Message{
			Role:    RoleUser,
			Content: "Summary of the conversation before this part:\n" + prior,
		})
	}
	for _, m := range a.History[from:to] {
		msgs = append(msgs, m)
	}
	msgs = append(msgs, Message{Role: RoleUser, Content: summaryInstruction(instructions)})
	return msgs
}

// streamSummary sends the summarization request. It carries no tool definitions,
// so the model can only answer with text, and the answer is not appended to the
// conversation. The turn's retry policy applies unchanged; the only difference is
// that a retry is not reported, since a retrying summarization is not a retrying
// turn and must not look like one in the transcript.
func (a *Agent) streamSummary(ctx context.Context, msgs []Message) (string, error) {
	send := func() (Message, error) {
		return a.Provider.Stream(ctx, msgs, nil, nil)
	}
	msg, err := a.retryTransient(ctx, send, nil, nil)
	if err != nil {
		return "", err
	}
	return msg.Content, nil
}

// summaryInstruction is the prompt that shapes the summary. It states what to
// record and, deliberately, that the summary is a record rather than a plan.
func summaryInstruction(instructions string) string {
	var b strings.Builder
	b.WriteString(`Summarize the conversation above so that it can be continued without the original messages.

Record, factually and compactly:
- what the user asked for, including constraints and preferences;
- decisions taken and the reasons for them;
- files read, created and modified, with their paths;
- commands run and what they showed, including failures and how they were resolved;
- errors encountered and the fixes applied;
- the current state of the work and what remains to be done;
- anything the user asked to remember or to avoid.

This is a record of what happened, not a plan: do not claim work is finished when it is not, and do not invent progress. Reply with the summary only.`)
	if s := strings.TrimSpace(instructions); s != "" {
		b.WriteString("\n\nThe user asked for this to be preserved or emphasized: " + s)
	}
	return b.String()
}
