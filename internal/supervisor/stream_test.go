package supervisor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

// consume runs the stream through consumeStream against a fresh job dir and
// returns the dir, an out buffer standing in for the job's out file, and the
// outcome.
func consume(t *testing.T, stream string) (dir string, out *bytes.Buffer, oc streamOutcome) {
	t.Helper()
	dir = t.TempDir()
	out = &bytes.Buffer{}
	oc = consumeStream(dir, strings.NewReader(stream), out)
	return dir, out, oc
}

func readProgress(t *testing.T, dir string) jobstore.Progress {
	t.Helper()
	p, ok := jobstore.ReadProgressDir(dir)
	if !ok {
		t.Fatal("progress file missing or undecodable")
	}
	return p
}

const goodStream = `{"event":"init","conversation_id":"c-1","init":{"cwd":"/tmp/x"}}
{"event":"step_update","step_update":{"step_index":0,"state":"DONE","step_type":"user_input"}}
{"event":"step_update","step_update":{"step_index":1,"state":"DONE","step_type":"agent_response","text_delta":"the answer"}}
{"event":"result","result":{"conversation_id":"c-1","status":"SUCCESS","response":"the answer","num_turns":1,"usage":{"total_tokens":42}}}
`

func TestConsumeStreamHappyPath(t *testing.T) {
	dir, out, oc := consume(t, goodStream)

	if oc.result == nil || oc.result.Response != "the answer" {
		t.Fatalf("result = %+v", oc.result)
	}
	if oc.malformed != 0 {
		t.Fatalf("malformed = %d", oc.malformed)
	}
	if got := out.String(); got != "the answer" {
		t.Fatalf("out = %q, want only the agent_response deltas", got)
	}
	// The progress file must name the conversation and the step reached, which is
	// what makes both readable while the job is still running.
	p := readProgress(t, dir)
	if p.ConversationID != "c-1" || p.StepIndex != 1 || p.StepType != streamjson.StepTypeAgentResponse {
		t.Fatalf("progress = %+v", p)
	}
	if p.UpdatedAt.IsZero() {
		t.Fatal("progress must carry a timestamp")
	}
}

// Only agent_response deltas make up the response. Tool calls and checkpoints
// carry progress but must not be mixed into the answer.
func TestConsumeStreamOnlyAppendsAgentResponse(t *testing.T) {
	stream := `{"event":"init","conversation_id":"c-1"}
{"event":"step_update","step_update":{"step_index":0,"step_type":"tool_call","text_delta":"SHOULD NOT APPEAR"}}
{"event":"step_update","step_update":{"step_index":1,"step_type":"agent_response","text_delta":"kept"}}
{"event":"step_update","step_update":{"step_index":2,"step_type":"checkpoint","text_delta":"NOR THIS"}}
`
	_, out, oc := consume(t, stream)
	if got := out.String(); got != "kept" {
		t.Fatalf("out = %q, want only the agent_response text", got)
	}
	// Positive control. Without it a typo in either "must not appear" line makes
	// this pass for the wrong reason: an undecodable line is skipped, so its text
	// never reaches out and the absence assertions hold vacuously.
	if oc.malformed != 0 {
		t.Fatalf("malformed = %d, want 0; a skipped line would satisfy the absence checks without proving anything", oc.malformed)
	}
}

// Several agent_response steps accumulate, so an interrupted multi-step run
// still yields everything the assistant said.
func TestConsumeStreamAccumulatesDeltas(t *testing.T) {
	stream := `{"event":"init","conversation_id":"c-1"}
{"event":"step_update","step_update":{"step_index":0,"step_type":"agent_response","text_delta":"one "}}
{"event":"step_update","step_update":{"step_index":1,"step_type":"agent_response","text_delta":"two "}}
{"event":"step_update","step_update":{"step_index":2,"step_type":"agent_response","text_delta":"three"}}
`
	_, out, oc := consume(t, stream)
	if got := out.String(); got != "one two three" {
		t.Fatalf("out = %q", got)
	}
	if oc.result != nil {
		t.Fatal("a stream with no result event must report none")
	}
}

// A run cut off before its result event still leaves its conversation behind,
// which is what lets the manager report a continuable partial result. This
// fixture's cut lands mid-line, so the truncated step contributes no text: the
// line is counted as malformed rather than decoded.
func TestConsumeStreamInterruptedKeepsPartial(t *testing.T) {
	stream := `{"event":"init","conversation_id":"c-9"}
{"event":"step_update","step_update":{"step_index":0,"step_type":"agent_response","text_delta":"half an ans`
	dir, out, oc := consume(t, stream)
	if oc.result != nil {
		t.Fatal("no terminal result should have been decoded")
	}
	if out.Len() != 0 {
		t.Fatalf("out = %q; a truncated line is not a decodable step", out.String())
	}
	// The count is the only observable separating "cut mid-line" from "ended
	// cleanly with no result", which the accumulates-deltas test already covers,
	// and it is what makes persist emit its skipped-lines note.
	if oc.malformed != 1 {
		t.Fatalf("malformed = %d, want 1 for the truncated tail", oc.malformed)
	}
	if p := readProgress(t, dir); p.ConversationID != "c-9" {
		t.Fatalf("progress = %+v, want the conversation recorded before the cut", p)
	}
}

// One corrupt line is counted and skipped; the result after it still lands.
func TestConsumeStreamCountsMalformed(t *testing.T) {
	stream := `{"event":"init","conversation_id":"c-1"}
not json at all
{"event":"result","result":{"conversation_id":"c-1","status":"SUCCESS","response":"ok"}}
`
	_, _, oc := consume(t, stream)
	if oc.malformed != 1 {
		t.Fatalf("malformed = %d, want 1", oc.malformed)
	}
	if oc.result == nil || oc.result.Response != "ok" {
		t.Fatalf("the result after a corrupt line must survive: %+v", oc.result)
	}
}

func TestPersistWritesResultAndNotesMalformed(t *testing.T) {
	dir := t.TempDir()
	var errBuf bytes.Buffer
	oc := streamOutcome{
		result:    &streamjson.Result{ConversationID: "c-1", Status: streamjson.StatusSuccess, Response: "done"},
		malformed: 3,
	}
	oc.persist(dir, &errBuf)

	b, rerr := jobstore.ReadResultDir(dir)
	if rerr != nil || b == nil {
		t.Fatal("result payload not written")
	}
	var res streamjson.Result
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("result payload: %v", err)
	}
	if res.Response != "done" || res.ConversationID != "c-1" {
		t.Fatalf("result = %+v", res)
	}
	// Skipped lines must be visible: silently dropping output would make a
	// truncated answer look complete.
	if !strings.Contains(errBuf.String(), "skipped 3 unreadable line") {
		t.Fatalf("stderr = %q, want it to report the skipped lines", errBuf.String())
	}
}

// No terminal result means no result file, which is exactly the signal the
// manager uses to mark a job's output partial.
func TestPersistWritesNoResultFileWhenNoneReached(t *testing.T) {
	dir := t.TempDir()
	var errBuf bytes.Buffer
	streamOutcome{}.persist(dir, &errBuf)

	if b, _ := jobstore.ReadResultDir(dir); b != nil {
		t.Fatal("a run with no terminal result must not write a result file")
	}
	if _, err := os.Stat(filepath.Join(dir, jobstore.ResultFile)); !os.IsNotExist(err) {
		t.Fatalf("result file should be absent, stat err = %v", err)
	}
	if errBuf.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing for a clean partial run", errBuf.String())
	}
}
