package streamjson

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// A verbatim capture from agy 1.1.8, trimmed of the init event's long tool list.
const sample = `{"event":"init","conversation_id":"21f0f78b-95b7-4aa5-a438-85a5078eac48","init":{"cwd":"/tmp/x","tools":["view_file"],"permission_mode":"request-review"}}
{"event":"step_update","step_update":{"conversation_id":"21f0f78b-95b7-4aa5-a438-85a5078eac48","step_index":0,"state":"DONE","step_type":"user_input"}}
{"event":"step_update","step_update":{"conversation_id":"21f0f78b-95b7-4aa5-a438-85a5078eac48","step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"pang\n","duration_seconds":3.55,"usage":{"input_tokens":7797,"output_tokens":278,"thinking_tokens":277,"cache_read_tokens":12172,"total_tokens":8075}}}
{"event":"result","result":{"conversation_id":"21f0f78b-95b7-4aa5-a438-85a5078eac48","status":"SUCCESS","response":"pang\n","duration_seconds":4.04,"num_turns":1,"usage":{"input_tokens":7896,"output_tokens":282,"total_tokens":8178}}}
`

// bufioDefaultBytes is bufio's own default buffer size, which NewReader takes.
// Several fixtures here have to sit either side of it, since it is the boundary
// between a line readLine hands back in place and one it accumulates. It is
// spelled out rather than assumed so those fixtures say which side they are on.
const bufioDefaultBytes = 4096

// The terminal result event, the shape most fixtures here need.
// TestLineExactlyAtCapDecodes uses the two halves directly, having to count them.
const (
	resultPrefix = `{"event":"result","result":{"status":"SUCCESS","response":"`
	resultSuffix = `"}}`
)

func resultLine(payload string) string { return resultPrefix + payload + resultSuffix + "\n" }

// readAll drains a reader and returns its events. The malformed count is read
// separately, off the Reader.
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
// case bufio.Scanner would abort the whole stream on, and the one that drives
// readLine's accumulation path.
func TestLongLineBeyondBufioBuffer(t *testing.T) {
	big := strings.Repeat("x", 128*bufioDefaultBytes)
	r := NewReader(strings.NewReader(resultLine(big)))
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

// The fixtures below are sized either side of the read buffer, so what that
// buffer actually is has to be checked rather than assumed: raising it would
// leave every one of them passing while silently testing the wrong path.
func TestReadBufferIsBufioDefault(t *testing.T) {
	if got := NewReader(strings.NewReader("")).br.Size(); got != bufioDefaultBytes {
		t.Fatalf("read buffer is %d bytes; the fixtures here are sized against %d", got, bufioDefaultBytes)
	}
}

// The fast path is this decoder's core claim, and nothing else can observe it:
// a copy decodes to exactly the same event, only slower, so both the suite and
// the benchmark are blind to losing it.
//
// What identifies bufio's own slice is where it ends. ReadSlice returns a view
// running from the read offset to the end of that buffer, so the capacity is the
// buffer size minus everything consumed before it, and consecutive lines hand
// back shrinking capacities that sum with their offsets to exactly Size(). No
// copy reproduces that: it lands in an array sized by the allocator or by the
// scratch's growth history, neither of which tracks the read offset. Watching
// the bytes change instead is NOT enough, and was the first thing tried here: a
// copy into a scratch that gets reused changes on the next line too.
//
// The scratch must also stay untouched, since the whole point of this path is
// that it accumulates nothing.
func TestShortLineIsReturnedInPlace(t *testing.T) {
	const line = `{"event":"init","conversation_id":"c1"}` + "\n"
	r := NewReader(strings.NewReader(line + line))
	first, truncated, err := r.readLine()
	if err != nil || truncated {
		t.Fatalf("first readLine: truncated=%v err=%v", truncated, err)
	}
	second, _, err := r.readLine()
	if err != nil {
		t.Fatalf("second readLine: %v", err)
	}
	if string(first) != line || string(second) != line {
		t.Fatalf("lines = %q, %q, want %q twice", first, second, line)
	}
	if r.buf != nil {
		t.Errorf("the fast path grew the scratch to %d bytes; it must accumulate nothing", cap(r.buf))
	}
	// Sizes rather than contents: a line returned in place ends where the read
	// buffer ends, and the second starts where the first left off.
	// A failure here means either that readLine copied the line or that the read
	// buffer is no longer the default; TestReadBufferIsBufioDefault says which.
	if cap(first) != bufioDefaultBytes {
		t.Errorf("first line has capacity %d, want the whole read buffer (%d)",
			cap(first), bufioDefaultBytes)
	}
	if want := bufioDefaultBytes - len(first); cap(second) != want {
		t.Errorf("second line has capacity %d, want %d, the buffer less the line before it",
			cap(second), want)
	}
}

// errAfter yields s and then fails with err on every later read, which is what a
// pipe torn down mid-run does. Next's contract is that such a stream is reported
// as the failure it is rather than passed off as a finished one, and that
// promise reaches the caller through three separate arms.
type errAfter struct {
	s   string
	err error
}

func (e *errAfter) Read(p []byte) (int, error) {
	if e.s == "" {
		return 0, e.err
	}
	n := copy(p, e.s)
	e.s = e.s[n:]
	return n, nil
}

// A read failure must never surface as io.EOF: the supervisor classifies a job
// by whether its stream ended cleanly, so a torn pipe reported as a clean end is
// a truncated answer presented as a complete one. Next carries the error out
// through three separate arms and all three have to, so the rows are named for
// the arm each one reaches, which is decided by what the stream holds when the
// read fails. Verified one at a time: each row is the sole killer of its arm,
// and this is the only test in the package that notices any of them.
func TestReadFailureIsNotReportedAsACleanEnd(t *testing.T) {
	want := errors.New("connection reset by peer")
	for _, tc := range []struct {
		name      string
		tail      string
		malformed int
	}{
		// Nothing follows the last terminator, so the failing read returns no bytes
		// and the empty line reaches the blank-line arm.
		{"the blank-line arm", "", 0},
		{"the decode-failure arm", `{"event":"result","result":{`, 1},
		{"the truncated arm", strings.Repeat("y", maxLineBytes+1024), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(&errAfter{s: resultLine("done") + tc.tail, err: want})
			ev, err := r.Next()
			if err != nil {
				t.Fatalf("first Next: %v", err)
			}
			if ev.Result == nil || ev.Result.Response != "done" {
				t.Fatalf("first event = %+v, want the complete line that preceded the failure", ev)
			}
			if _, err := r.Next(); !errors.Is(err, want) {
				t.Fatalf("Next err = %v, want the read failure", err)
			}
			if r.Malformed() != tc.malformed {
				t.Fatalf("malformed = %d, want %d", r.Malformed(), tc.malformed)
			}
		})
	}
}

// Decoding happens inside a buffer that the next read overwrites, so a payload
// field that aliased it rather than copying would corrupt events already handed
// to the caller, and only once the stream outran the buffer. The supervisor
// keeps a *Result past the loop and a StepType across iterations, so that
// corruption would land in the answer a job reports. Every field is a string or
// a number today, which is why this holds; the test is here so that adding a
// []byte or a json.RawMessage to Event fails loudly instead of silently.
func TestDecodedEventsSurviveLaterReads(t *testing.T) {
	const lines = 400
	var sb strings.Builder
	for i := range lines {
		fmt.Fprintf(&sb, `{"event":"result","result":{"conversation_id":"c-%d","status":"SUCCESS","response":"r-%d"}}`+"\n", i, i)
	}
	if sb.Len() < 4*bufioDefaultBytes {
		t.Fatalf("fixture is %d bytes, too small to reuse the read buffer", sb.Len())
	}
	evs := readAll(t, NewReader(strings.NewReader(sb.String())))
	if len(evs) != lines {
		t.Fatalf("got %d events, want %d", len(evs), lines)
	}
	// Read back only after the whole stream has been drained, so every earlier
	// event has had the buffer beneath it rewritten many times over.
	for i, ev := range evs {
		if ev.Result == nil {
			t.Fatalf("event %d has no result payload", i)
		}
		wantID, wantResp := fmt.Sprintf("c-%d", i), fmt.Sprintf("r-%d", i)
		if ev.Result.ConversationID != wantID || ev.Result.Response != wantResp {
			t.Fatalf("event %d = %q/%q after the stream was drained, want %q/%q",
				i, ev.Result.ConversationID, ev.Result.Response, wantID, wantResp)
		}
	}
}

// Past the retention cap a line is dropped rather than decoded from a prefix,
// and the stream keeps going.
func TestOverlongLineIsDroppedNotDecoded(t *testing.T) {
	overlong := resultLine(strings.Repeat("y", maxLineBytes+1024))
	after := resultLine("after")
	r := NewReader(strings.NewReader(overlong + after))
	evs := readAll(t, r)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want only the line that fits", len(evs))
	}
	if evs[0].Result == nil || evs[0].Result.Response != "after" {
		t.Fatalf("event = %+v, want the line following the over-long one", evs[0])
	}
	if r.Malformed() != 1 {
		t.Fatalf("malformed = %d, want 1", r.Malformed())
	}
}

// The whole reason maxLineBytes exists is to stop a corrupt or unterminated
// stream growing the supervisor's memory without bound, and until this test
// nothing checked it: deleting the clamp on the chunk that crosses the cap left
// every other test in the package green while the reader retained the entire
// stream. Asserting the retained length, rather than only that the line was
// dropped, is what makes the bound observable.
// Run over two chunk sizes, because they decide how the clamp is reached. The
// default divides the cap, so the accumulation lands on it exactly and the clamp
// keeps nothing. A caller may also hand NewReader a *bufio.Reader that is
// already bigger than the default, which bufio then keeps as-is; pick a size
// that does not divide the cap and the crossing chunk has to be cut. Both must
// retain the same thing.
func TestOverlongLineRetainsOnlyTheCap(t *testing.T) {
	// Not a divisor of maxLineBytes, so the cap falls inside a chunk.
	const straddlingBufBytes = 100_000
	const overshoot = 64 * bufioDefaultBytes
	for _, tc := range []struct {
		name string
		wrap func(io.Reader) io.Reader
	}{
		{"chunks that divide the cap", func(r io.Reader) io.Reader { return r }},
		{"chunks that straddle the cap", func(r io.Reader) io.Reader {
			return bufio.NewReaderSize(r, straddlingBufBytes)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.NewReader(strings.Repeat("y", maxLineBytes+overshoot) + "\n")
			r := NewReader(tc.wrap(src))
			line, truncated, err := r.readLine()
			if err != nil {
				t.Fatalf("readLine: %v", err)
			}
			if !truncated {
				t.Fatal("a line past the cap must come back reported as truncated")
			}
			if len(line) != maxLineBytes {
				t.Errorf("retained %d bytes, want exactly the cap (%d)", len(line), maxLineBytes)
			}
			if r.buf != nil {
				t.Errorf("a truncated line left %d bytes of scratch pinned for the rest of the run", cap(r.buf))
			}
		})
	}
}

// The cap is on payload, not on payload plus terminator: a line of exactly
// maxLineBytes followed by a newline is the longest legal line, not the first
// illegal one.
func TestLineExactlyAtCapDecodes(t *testing.T) {
	pad := maxLineBytes - len(resultPrefix) - len(resultSuffix)
	line := resultLine(strings.Repeat("z", pad))
	if got := len(line) - 1; got != maxLineBytes {
		t.Fatalf("fixture payload is %d bytes, want exactly %d", got, maxLineBytes)
	}
	r := NewReader(strings.NewReader(line))
	evs := readAll(t, r)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want the line at the cap to decode", len(evs))
	}
	if evs[0].Result == nil || len(evs[0].Result.Response) != pad {
		t.Fatalf("response is %d bytes, want %d", len(evs[0].Result.Response), pad)
	}
	if r.Malformed() != 0 {
		t.Fatalf("malformed = %d, want 0 at exactly the cap", r.Malformed())
	}
}

// An over-long line made entirely of whitespace is corruption, not padding.
// Malformed's contract is "counted whenever it exceeded maxLineBytes or failed
// to decode", and the blank-line carve-out must not quietly except this shape.
func TestOverlongBlankLineIsCounted(t *testing.T) {
	in := strings.Repeat(" ", maxLineBytes+1024) + "\n" + resultLine("after")
	r := NewReader(strings.NewReader(in))
	if evs := readAll(t, r); len(evs) != 1 {
		t.Fatalf("got %d events, want the line after the over-long one", len(evs))
	}
	if r.Malformed() != 1 {
		t.Fatalf("malformed = %d, want 1: an over-long line is not padding", r.Malformed())
	}
}

// A line that fails to decode on the same call that ends the stream must still
// be counted. The count is what the supervisor reports as "skipped N unreadable
// line(s)", and agy's last line is the one that matters, so a truncated final
// line silently vanishing is exactly the case worth pinning: bufio hands back
// the buffered bytes together with io.EOF when the stream ends without a
// terminator.
func TestDecodeFailureArrivingWithEndOfStream(t *testing.T) {
	r := NewReader(strings.NewReader(`{"event":"result","result":{`))
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next err = %v, want io.EOF", err)
	}
	if r.Malformed() != 1 {
		t.Fatalf("malformed = %d, want 1 for an undecodable final line", r.Malformed())
	}
}

// stallAfter yields s and then reports neither data nor an error, ever, which is
// a reader that has simply stopped.
type stallAfter struct{ s string }

func (e *stallAfter) Read(p []byte) (int, error) {
	if e.s == "" {
		return 0, nil
	}
	n := copy(p, e.s)
	e.s = e.s[n:]
	return n, nil
}

// What Next's doc says happens AFTER a stream fails, which until now was
// asserted only in that doc: the error keeps coming back rather than being
// cleared once it has been handed out, and a reader that stalls is reported as
// no progress instead of as a clean end. The manager polls a job repeatedly, so
// the second answer matters as much as the first.
func TestNextKeepsReportingAFailedStream(t *testing.T) {
	want := errors.New("connection reset by peer")
	torn := NewReader(&errAfter{s: resultLine("done"), err: want})
	if _, err := torn.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	for i := range 4 {
		if _, err := torn.Next(); !errors.Is(err, want) {
			t.Fatalf("Next %d after the failure = %v, want the failure again", i+2, err)
		}
	}

	stalled := NewReader(&stallAfter{s: resultLine("done")})
	if _, err := stalled.Next(); err != nil {
		t.Fatalf("first Next on the stalled reader: %v", err)
	}
	if _, err := stalled.Next(); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("Next on a stalled reader = %v, want io.ErrNoProgress", err)
	}
}

// The same coincidence one branch over: a line that ends the stream by running
// past the cap rather than by failing to decode. This is the shape a killed agy
// leaves behind mid-answer, and it is the only arm of Next that no other test
// here reaches.
func TestOverlongUnterminatedLineIsCounted(t *testing.T) {
	r := NewReader(strings.NewReader(strings.Repeat("y", maxLineBytes+1024)))
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next err = %v, want io.EOF", err)
	}
	if r.Malformed() != 1 {
		t.Fatalf("malformed = %d, want 1 for an over-long final line", r.Malformed())
	}
}

// The decoder's input is a live subprocess's stdout, so it is not a trusted
// document: it can be interleaved, cut mid-line, or carry whatever a tool
// printed. Its contract under all of that is narrow enough to state, and this
// asserts the whole of it, since the shapes that break a hand-written table are
// exactly the ones nobody thinks to write down.
//
// The tally is an equality rather than a bound. A bound is satisfied by a
// decoder that yields nothing at all, which is precisely the regression worth
// catching, since every remaining assertion here is about what Next does NOT do.
//
// The precedent for fuzzing rather than adding more example rows is agyver:
// five rounds of review-driven fixes to that parser were each contradicted by
// measurement, and its fuzz target plus a 40-row table is what settled it.
func FuzzNextSurvivesArbitraryStreams(f *testing.F) {
	f.Add(sample)
	f.Add("")
	f.Add("\n\n\n")
	f.Add("   ")
	f.Add("not json")
	f.Add(`{"event":"result","result":{`)
	f.Add("{\"event\":\"init\"}\n\x00\xff\n{\"event\":\"result\"}")
	f.Add(`{"event":"step_update","step_update":{"step_index":9,"text_delta":"x"}}`)
	f.Add(`{"event":"init","init":{"tools":["a","b"]},"conversation_id":"c"}` + "\n")
	f.Add(strings.Repeat("{", 5000))
	f.Add(strings.Repeat("\n", 5000))
	// Two seeds past the read buffer, so the corpus covers readLine's
	// accumulation path deliberately rather than by the accident of one earlier
	// seed being long: one of these decodes and one cannot.
	f.Add(resultLine(strings.Repeat("x", 2*bufioDefaultBytes)))
	f.Add(strings.Repeat("x", 2*bufioDefaultBytes) + "\n" + resultLine("after"))

	f.Fuzz(func(t *testing.T, in string) {
		r := NewReader(strings.NewReader(in))
		events := 0
		for {
			ev, err := r.Next()
			if err != nil {
				// A strings.Reader has no failure mode but the end of its string, so
				// any other error here is the decoder inventing one.
				if !errors.Is(err, io.EOF) {
					t.Fatalf("Next err = %v, want io.EOF on a strings.Reader", err)
				}
				break
			}
			events++
			// Whatever decoded, reading the id off it must not panic: production
			// calls this on every event before looking at the kind.
			_ = ev.ConversationIDOf()
			if events > len(in)+1 {
				t.Fatalf("%d events from %d bytes: Next is not consuming input", events, len(in))
			}
		}
		// Every line is decoded, counted malformed, or forgiven as padding, and
		// never two of those, so the two tallies together account for exactly the
		// lines that were not padding. An over-long line is never padding, whatever
		// its bytes.
		want := 0
		for line := range strings.SplitSeq(in, "\n") {
			if strings.TrimSpace(line) != "" || len(line) > maxLineBytes {
				want++
			}
		}
		if got := events + r.Malformed(); got != want {
			t.Fatalf("events=%d + malformed=%d = %d, want one outcome for each of the %d non-padding line(s)",
				events, r.Malformed(), got, want)
		}
	})
}

// Framing must be independent of payload: a text_delta is the model's own
// output and can hold anything, including the newline this format delimits on
// (JSON escapes it, so the line stays one line) and the read boundary.
//
// The events are padded and interleaved with a line known to be undecodable, so
// what is asserted is where the decoder cuts the stream and what it refuses,
// rather than encoding/json's ability to round-trip its own output.
func FuzzFramingPreservesPayloads(f *testing.F) {
	f.Add("pang\n", "21f0f78b-95b7-4aa5-a438-85a5078eac48")
	f.Add("", "")
	f.Add("line one\nline two\r\n{\"event\":\"result\"}\n", "c1")
	f.Add(strings.Repeat("x", bufioDefaultBytes+7), "c2")
	f.Add("\t \x00 ünïcødé 🐦", "c3")

	f.Fuzz(func(t *testing.T, delta, conv string) {
		// json.Marshal replaces invalid UTF-8 with U+FFFD, so a byte-for-byte
		// comparison would fail on the encoder's behaviour, not the decoder's.
		if !utf8.ValidString(delta) || !utf8.ValidString(conv) {
			t.Skip("not valid UTF-8")
		}
		want := []Event{
			{Kind: EventInit, ConversationID: conv, Init: &Init{Cwd: "/tmp/x"}},
			{Kind: EventStepUpdate, StepUpdate: &StepUpdate{
				ConversationID: conv, StepIndex: 3, StepType: StepTypeAgentResponse, TextDelta: delta,
			}},
			{Kind: EventResult, Result: &Result{ConversationID: conv, Status: StatusSuccess, Response: delta}},
		}
		const garbage = `{"event":"result",`
		var stream strings.Builder
		for _, ev := range want {
			b, err := json.Marshal(ev)
			if err != nil {
				t.Fatalf("marshalling the fixture: %v", err)
			}
			if len(b) > maxLineBytes {
				t.Skip("past the retention cap, where dropping the line is the contract")
			}
			stream.WriteString("\n")
			stream.Write(b)
			stream.WriteString("\n" + garbage + "\n")
		}

		r := NewReader(strings.NewReader(stream.String()))
		got := readAll(t, r)
		if r.Malformed() != len(want) {
			t.Fatalf("malformed = %d, want one per interleaved garbage line (%d)", r.Malformed(), len(want))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decoded events differ from the ones written:\n got %+v\nwant %+v", got, want)
		}
	})
}

// initWithToolList is agy's init event as it really arrives, carrying the tool
// list this package deliberately declines to decode. The sample capture above is
// trimmed to one tool, which understates that decision by a factor of the tool
// count, so the benchmark measures both shapes.
func initWithToolList(n int) string {
	tools := make([]string, n)
	for i := range tools {
		tools[i] = fmt.Sprintf(`"tool_number_%02d"`, i)
	}
	return `{"event":"init","conversation_id":"21f0f78b-95b7-4aa5-a438-85a5078eac48",` +
		`"init":{"cwd":"/tmp/x","tools":[` + strings.Join(tools, ",") +
		`],"permission_mode":"request-review"}}` + "\n"
}

// BenchmarkNext measures the per-line decode cost over the three line shapes
// that take different paths: short lines returned from bufio's buffer and
// decoded in place, an init event whose tool list is skipped rather than
// decoded, and a line past the read buffer that has to be accumulated into the
// Reader's scratch.
//
// The decoded count is asserted, because the loop's terminating error is
// otherwise indistinguishable from a decoder that returns io.EOF immediately,
// which benchmarks as a hundredfold improvement. Each iteration also constructs
// a Reader, whose read buffer is inside the reported B/op; that is amortized
// over the fixture's lines but it is not nothing.
func BenchmarkNext(b *testing.B) {
	for _, bm := range []struct {
		name   string
		stream string
		events int
	}{
		{"short lines", strings.Repeat(sample, 64), 4 * 64},
		{"init with a full tool list", strings.Repeat(initWithToolList(60), 64), 64},
		{"lines past the read buffer", strings.Repeat(resultLine(strings.Repeat("x", 5*bufioDefaultBytes)), 64), 64},
	} {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(bm.stream)))
			for b.Loop() {
				r := NewReader(strings.NewReader(bm.stream))
				n := 0
				for {
					if _, err := r.Next(); err != nil {
						if !errors.Is(err, io.EOF) {
							b.Fatalf("Next: %v", err)
						}
						break
					}
					n++
				}
				if n != bm.events {
					b.Fatalf("decoded %d events, want %d", n, bm.events)
				}
			}
		})
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
