package streamjson

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// A verbatim capture from agy 1.1.8, trimmed of the init event's long tool list.
const sample = `{"event":"init","conversation_id":"21f0f78b-95b7-4aa5-a438-85a5078eac48","init":{"cwd":"/tmp/x","tools":["view_file"],"permission_mode":"request-review"}}
{"event":"step_update","step_update":{"conversation_id":"21f0f78b-95b7-4aa5-a438-85a5078eac48","step_index":0,"state":"DONE","step_type":"user_input"}}
{"event":"step_update","step_update":{"conversation_id":"21f0f78b-95b7-4aa5-a438-85a5078eac48","step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"pang\n","duration_seconds":3.55,"usage":{"input_tokens":7797,"output_tokens":278,"thinking_tokens":277,"cache_read_tokens":12172,"total_tokens":8075}}}
{"event":"result","result":{"conversation_id":"21f0f78b-95b7-4aa5-a438-85a5078eac48","status":"SUCCESS","response":"pang\n","duration_seconds":4.04,"num_turns":1,"usage":{"input_tokens":7896,"output_tokens":282,"total_tokens":8178}}}
`

// readAll drains a reader, returning the events and the malformed count.
func readAll(t *testing.T, r *Reader) []Event {
	t.Helper()
	var evs []Event
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return evs
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		evs = append(evs, ev)
	}
}

func TestReadsRealCapture(t *testing.T) {
	r := NewReader(strings.NewReader(sample))
	evs := readAll(t, r)
	if len(evs) != 4 {
		t.Fatalf("got %d events, want 4", len(evs))
	}
	if r.Malformed() != 0 {
		t.Fatalf("malformed = %d in a clean stream", r.Malformed())
	}
	const conv = "21f0f78b-95b7-4aa5-a438-85a5078eac48"
	if evs[0].Kind != EventInit || evs[0].ConversationID != conv {
		t.Fatalf("init event = %+v", evs[0])
	}
	if evs[0].Init == nil || evs[0].Init.Cwd != "/tmp/x" {
		t.Fatalf("init payload = %+v", evs[0].Init)
	}
	su := evs[2].StepUpdate
	if su == nil || su.StepType != StepTypeAgentResponse || su.TextDelta != "pang\n" {
		t.Fatalf("agent_response step = %+v", su)
	}
	if su.Usage == nil || su.Usage.CacheReadTokens != 12172 {
		t.Fatalf("step usage = %+v", su.Usage)
	}
	res := evs[3].Result
	if res == nil || res.Status != StatusSuccess || res.Response != "pang\n" || res.NumTurns != 1 {
		t.Fatalf("result = %+v", res)
	}
}

// Every event kind must surface the conversation, wherever it puts it: the init
// event at the top level, the others inside their payload.
func TestConversationIDOf(t *testing.T) {
	r := NewReader(strings.NewReader(sample))
	for i, ev := range readAll(t, r) {
		if got := ev.ConversationIDOf(); got != "21f0f78b-95b7-4aa5-a438-85a5078eac48" {
			t.Errorf("event %d (%s) conversation = %q", i, ev.Kind, got)
		}
	}
	if got := (Event{Kind: EventInit}).ConversationIDOf(); got != "" {
		t.Errorf("an event with no conversation = %q, want empty", got)
	}
}

// One unreadable line must not discard the events after it. Losing the terminal
// result to a single corrupt line would turn a complete run into a partial one.
func TestSkipsMalformedLinesAndKeepsResult(t *testing.T) {
	in := `{"event":"init","conversation_id":"c1"}
this is not json
{"event":"step_update",  <-- truncated
{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"done"}}
`
	r := NewReader(strings.NewReader(in))
	evs := readAll(t, r)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want the init and the result", len(evs))
	}
	if r.Malformed() != 2 {
		t.Fatalf("malformed = %d, want 2", r.Malformed())
	}
	if evs[1].Result == nil || evs[1].Result.Response != "done" {
		t.Fatalf("the terminal result must survive a corrupt line: %+v", evs[1].Result)
	}
}

// Blank lines are padding, not corruption, and must not inflate the malformed
// count (which the supervisor reports to the user).
func TestBlankLinesAreNotMalformed(t *testing.T) {
	in := "\n\n{\"event\":\"init\",\"conversation_id\":\"c1\"}\n\n   \n"
	r := NewReader(strings.NewReader(in))
	if evs := readAll(t, r); len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if r.Malformed() != 0 {
		t.Fatalf("malformed = %d, want 0 for blank padding", r.Malformed())
	}
}

// A final line with no trailing newline is still a complete event: agy's result
// is the last thing on the stream and may arrive unterminated.
func TestUnterminatedFinalLine(t *testing.T) {
	in := `{"event":"result","result":{"status":"SUCCESS","response":"tail"}}`
	r := NewReader(strings.NewReader(in))
	evs := readAll(t, r)
	if len(evs) != 1 || evs[0].Result == nil || evs[0].Result.Response != "tail" {
		t.Fatalf("events = %+v", evs)
	}
}

// A line longer than bufio's internal buffer must decode, not fail: the init
// event's tool list and a large text_delta both routinely exceed it. This is the
// case bufio.Scanner would abort the whole stream on.
func TestLongLineBeyondBufioBuffer(t *testing.T) {
	big := strings.Repeat("x", 512*1024)
	in := `{"event":"result","result":{"status":"SUCCESS","response":"` + big + `"}}` + "\n"
	r := NewReader(strings.NewReader(in))
	evs := readAll(t, r)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if evs[0].Result == nil || len(evs[0].Result.Response) != len(big) {
		t.Fatalf("long response truncated")
	}
	if r.Malformed() != 0 {
		t.Fatalf("malformed = %d, want 0", r.Malformed())
	}
}

// Past the retention cap a line is dropped rather than decoded from a prefix,
// and the stream keeps going.
func TestOverlongLineIsDroppedNotDecoded(t *testing.T) {
	huge := strings.Repeat("y", maxLineBytes+1024)
	in := `{"event":"result","result":{"status":"SUCCESS","response":"` + huge + `"}}` + "\n" +
		`{"event":"result","result":{"status":"SUCCESS","response":"after"}}` + "\n"
	r := NewReader(strings.NewReader(in))
	evs := readAll(t, r)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want only the line that fits", len(evs))
	}
	if evs[0].Result.Response != "after" {
		t.Fatalf("response = %q, want the following line", evs[0].Result.Response)
	}
	if r.Malformed() != 1 {
		t.Fatalf("malformed = %d, want 1", r.Malformed())
	}
}

// An in-band failure decodes with its message intact; this is how agy reports an
// unresolvable model or its own print-timeout.
func TestErrorResult(t *testing.T) {
	in := `{"event":"result","result":{"conversation_id":"","status":"ERROR","response":"","error":"timeout waiting for response"}}`
	r := NewReader(strings.NewReader(in))
	evs := readAll(t, r)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	res := evs[0].Result
	if res.Status != StatusError || res.Error != "timeout waiting for response" {
		t.Fatalf("result = %+v", res)
	}
}
