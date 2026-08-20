package supervisor

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

// streamOutcome is what the supervisor learned from one run's stream-json
// output: the terminal result if the run reached one, and how many unusable
// lines were skipped along the way.
//
// It carries no conversation id of its own, because nothing outside the tests
// ever read one, and the id already leaves here by the two routes callers do
// read: live in progress.json, written as soon as the init event names it, which
// is what lets the manager report it mid-run; and after the fact inside
// result.json, since result carries agy's own ConversationID (the manager reads
// that as a fallback, see carryResultMetadata).
type streamOutcome struct {
	result    *streamjson.Result
	malformed int
}

// consumeStream decodes agy's event stream to completion, mirroring it into the
// job directory as it goes.
//
// Two things are written live rather than at the end, because a run can be
// cancelled or time out at any point and both must survive that:
//
//   - progress.json, so the manager can report the conversation id (carried by
//     the init event, which arrives before any model work) and the current step
//     while the job is still running.
//   - out, which accumulates the assistant's text. It is the fallback the
//     manager reports as a partial result when no terminal event ever arrives.
//     Only agent_response deltas contribute: agy's stream carries no thinking
//     text (only a usage.thinking_tokens counter) and a tool step carries no
//     response text, so a run killed while still reasoning or mid-tool, before
//     it streams any answer, leaves out empty by design, not by defect
//     (MEASURED against agy 1.1.15).
//
// The terminal result is returned rather than written here, so the caller can
// order it against the exit-code sentinel.
func consumeStream(jobDir string, r io.Reader, out io.Writer) streamOutcome {
	var oc streamOutcome
	sr := streamjson.NewReader(r)
	var prog jobstore.Progress
	for {
		ev, err := sr.Next()
		if err != nil {
			// io.EOF on a clean end, or a read error once the process group was
			// torn down. Either way there is nothing further to decode, and the
			// exit-code sentinel is what classifies the outcome.
			break
		}
		changed := false
		if cid := ev.ConversationIDOf(); cid != "" && cid != prog.ConversationID {
			prog.ConversationID = cid
			changed = true
		}
		switch ev.Kind {
		case streamjson.EventStepUpdate:
			if su := ev.StepUpdate; su != nil {
				// Only rewrite when something a reader can observe actually moved.
				// The conversation branch above already guards this way; without the
				// same guard here, a repeated update for one step (agy stamps its id
				// on every event, and a step can report more than one state) rewrites
				// a byte-identical file, and each rewrite is a full atomic replace.
				if su.StepIndex != prog.StepIndex || su.StepType != prog.StepType {
					prog.StepIndex = su.StepIndex
					prog.StepType = su.StepType
					changed = true
				}
				// Append every delta rather than tracking which steps have already
				// contributed. agy streams one agent_response step as many text_delta
				// events (the ACTIVE states carry successive chunks, the DONE state the
				// final tail), so appending every one in arrival order reproduces the
				// response: for a single-response run, concatenating its agent_response
				// deltas matched its terminal result byte-for-byte (MEASURED against agy
				// 1.1.15). A run
				// with several agent_response steps accumulates all of them, which is
				// more context than the terminal result carries. The manager reads it
				// when no terminal result reached disk, and also when one did but carried
				// no response of its own, since then this is the only text there is.
				//
				// Do NOT dedup by step_index to "skip repeats": the deltas are not
				// repeats and agy does not emit one per step, so a step-indexed filter
				// would drop every ACTIVE chunk and keep only the final tail.
				if su.StepType == streamjson.StepTypeAgentResponse && su.TextDelta != "" {
					// A failed append costs partial-result fidelity, nothing more: the
					// terminal result event is the authoritative response and is written
					// separately. Losing the stream would be worse than losing this text.
					_, _ = io.WriteString(out, su.TextDelta)
				}
			}
		case streamjson.EventResult:
			if ev.Result != nil {
				oc.result = ev.Result
			}
		}
		if changed {
			prog.UpdatedAt = time.Now().UTC()
			// Best effort for the same reason: progress is a hint the manager treats
			// as absent when unreadable, never a correctness gate.
			_ = jobstore.WriteProgressDir(jobDir, prog)
		}
	}
	oc.malformed = sr.Malformed()
	return oc
}

// persist writes the terminal result to the job directory and notes any skipped
// lines on stderr. A run that produced no terminal event writes no result file,
// which is exactly the signal the manager uses to mark its captured output
// partial.
func (oc streamOutcome) persist(jobDir string, errW io.Writer) {
	if oc.malformed > 0 {
		// Surface corruption where a reader will actually see it. Skipping a line
		// is recoverable, so it must not fail the job, but silently discarding
		// output would make a truncated answer look complete.
		_, _ = fmt.Fprintf(errW, "\nagy-mcp: skipped %d unreadable line(s) in agy's stream-json output\n", oc.malformed)
	}
	if oc.result == nil {
		return
	}
	b, err := json.Marshal(oc.result)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "\nagy-mcp: could not record agy's result payload: %v\n", err)
		return
	}
	if err := jobstore.WriteResultDir(jobDir, b); err != nil {
		_, _ = fmt.Fprintf(errW, "\nagy-mcp: could not write agy's result payload: %v\n", err)
	}
}
