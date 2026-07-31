// Package streamjson decodes agy's `--output-format stream-json` event stream:
// newline-delimited JSON carrying an init event, per-step updates, and one
// terminal result.
package streamjson

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Event kinds emitted by agy on the stream-json stream.
const (
	EventInit       = "init"
	EventStepUpdate = "step_update"
	EventResult     = "result"
)

// Terminal result statuses. agy reports ERROR in-band (with a populated Error)
// for failures it survives long enough to describe, such as an unresolvable
// model or its own print-timeout expiring.
const (
	StatusSuccess = "SUCCESS"
	StatusError   = "ERROR"
)

// StepTypeAgentResponse marks the step whose TextDelta values make up the
// assistant's answer. Other step types (tool calls, checkpoints) carry
// progress information but do not contribute to the response text.
const StepTypeAgentResponse = "agent_response"

// Usage is the token accounting agy attaches to steps and to the result.
type Usage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

// Init is the payload of the init event. Its arrival is what makes a run's
// conversation id known long before the run finishes.
//
// agy also puts its full tool list on this event. There is deliberately no field
// for it, because it is the one part of this payload whose cost scales: a string
// per tool, on every run, for a value nothing reads. A key with no field is
// skipped by the decoder without allocating its value, so ignoring it costs only
// the scan. Cwd and PermissionMode are unread too and stay anyway, being two
// fixed strings that record what the event carries.
type Init struct {
	Cwd            string `json:"cwd,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

// StepUpdate is the payload of a step_update event.
type StepUpdate struct {
	ConversationID  string  `json:"conversation_id,omitempty"`
	StepIndex       int     `json:"step_index"`
	State           string  `json:"state,omitempty"`
	StepType        string  `json:"step_type,omitempty"`
	TextDelta       string  `json:"text_delta,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Usage           *Usage  `json:"usage,omitempty"`
}

// Result is the payload of the terminal result event.
type Result struct {
	ConversationID  string  `json:"conversation_id,omitempty"`
	Status          string  `json:"status,omitempty"`
	Response        string  `json:"response,omitempty"`
	Error           string  `json:"error,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	NumTurns        int     `json:"num_turns,omitempty"`
	Usage           *Usage  `json:"usage,omitempty"`
}

// Event is one decoded line of the stream. agy sets exactly one payload pointer,
// matching Kind, and nothing here validates that, so production consumers
// nil-check the pointer for the kind they handle instead of trusting Kind on its
// own. An event whose pointer does not match its Kind is neither skipped nor
// counted in Malformed: Next returns it and the consumer's nil check drops it
// silently. A line is skipped and counted only when it fails to decode or when it
// overruns maxLineBytes, which is checked before any decode is attempted.
type Event struct {
	Kind string `json:"event"`
	// ConversationID is carried at the top level on the init event; the other
	// kinds repeat it inside their payload instead.
	ConversationID string      `json:"conversation_id,omitempty"`
	Init           *Init       `json:"init,omitempty"`
	StepUpdate     *StepUpdate `json:"step_update,omitempty"`
	Result         *Result     `json:"result,omitempty"`
}

// ConversationIDOf returns the conversation id an event carries, wherever the
// event kind happens to put it, or "" when it carries none.
func (e Event) ConversationIDOf() string {
	switch {
	case e.ConversationID != "":
		return e.ConversationID
	case e.StepUpdate != nil && e.StepUpdate.ConversationID != "":
		return e.StepUpdate.ConversationID
	case e.Result != nil && e.Result.ConversationID != "":
		return e.Result.ConversationID
	}
	return ""
}

// maxLineBytes caps how much payload one NDJSON line may carry. agy's init event
// embeds the full tool list and a step_update can carry a large text_delta, so
// the limit is generous; it exists only to keep a corrupt or unterminated
// stream from growing the supervisor's memory without bound. A longer line is
// truncated and reported as malformed rather than decoded from a partial
// prefix, which would be worse than dropping it. The '\n' is not payload and
// does not count against the cap, so a line of exactly this many bytes plus its
// terminator still decodes. On a \r\n stream the '\r' does count, costing that
// one shape a byte of headroom, which is not worth a branch on the read path.
const maxLineBytes = 8 << 20

// Reader decodes events from an agy stream-json stream.
//
// It skips blank and undecodable lines instead of aborting: the stream is a
// live process's stdout, and one unparseable line must not discard the events
// that follow it, least of all the terminal result. Skipped lines are counted
// so the caller can report them.
type Reader struct {
	br        *bufio.Reader
	malformed int
	// buf accumulates a line too long to arrive in one read, and is reused for
	// the rest of the stream: a run whose lines are long then pays one growing
	// allocation instead of one per line. A truncated line drops it rather than
	// keeping it, because corruption is not evidence that this run's lines are
	// large, and append's growth overshoots, so holding it would pin more than
	// maxLineBytes for the life of the job on the one input the cap exists to
	// contain. The cost of dropping it is paid by a stream of CONSECUTIVE
	// over-long lines, which then regrows from nil each time: the bytes through
	// the allocator go from constant to linear in the length of that run,
	// measured at 39.7 MiB per line against 39.7 MiB total. That is the trade
	// taken deliberately, since a bounded live set matters more than the
	// throughput of a stream that is already corrupt.
	// Nothing outside readLine may hold it past the next call.
	buf []byte
}

// NewReader returns a Reader decoding r.
//
// bufio's default buffer size is deliberate. A larger one lets more lines be
// decoded in place rather than accumulated, but measured against this decoder it
// buys nothing: at every line length up to 256 KiB, 64 KiB was within noise of
// the 4096 default on time and allocation count, because json.Unmarshal is 99%
// of the per-line cost. It only added its own size to the resident footprint of
// every running job.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReader(r)}
}

// Malformed returns how many lines have been skipped so far because they
// exceeded maxLineBytes or failed to decode. Blank lines are padding and are
// not counted, unless they were over-long: a line that ran past the cap is
// corruption whatever bytes survived in it. It is a running total, updated as
// the stream is consumed, so it is readable at any point and is final once Next
// has returned an error.
func (r *Reader) Malformed() int { return r.malformed }

// Next returns the next decodable event, io.EOF at a clean end of stream, or a read
// error, so a stream torn off mid-run is reported as the failure it is rather than
// passed off as a finished one. Along the way it skips blank lines silently, and
// skips undecodable and over-long lines after counting them in Malformed.
//
// A read error arriving on the same call as a decodable event is DEFERRED rather
// than returned with it: the event comes back first, which is what io.Reader asks
// of a caller (process the bytes, then consider the error), and the next call
// reports the failure. Nothing is swallowed. Measured both ways: a reader that
// repeats its error (a torn pipe) yields it again, and one that falls silent yields
// io.ErrNoProgress. The case is narrow anyway, since bufio hands back a buffered
// whole line with a nil error and keeps the error for the following read, so the
// two only coincide when the tear lands on a line with no terminator.
func (r *Reader) Next() (Event, error) {
	for {
		line, truncated, err := r.readLine()
		if truncated {
			// Counted before the line is so much as examined, which is what makes
			// Malformed's contract hold: a line that ran past the cap is corruption
			// whatever bytes survived in it, and the padding the blank case below
			// forgives is a short line by definition. Trimming first would also mean
			// scanning a discarded 8 MiB.
			r.malformed++
			if err != nil {
				return Event{}, err
			}
			continue
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			// A blank line is padding, not corruption; do not count it.
			if err != nil {
				return Event{}, err
			}
			continue
		}
		var ev Event
		if decErr := json.Unmarshal(trimmed, &ev); decErr != nil {
			r.malformed++
			if err != nil {
				return Event{}, err
			}
			continue
		}
		// Drop a concurrent io.EOF here rather than returning it alongside the
		// event: the caller would have to handle "value and EOF together", and
		// bufio.Reader re-reports EOF on the next call anyway.
		return ev, nil
	}
}

// readLine reads one line, growing past bufio's internal buffer. It returns the
// line, whether it exceeded maxLineBytes (in which case the overflow is
// discarded and the retained prefix must not be decoded), and the terminating
// error if any.
//
// The '\n' comes back attached only on the fast path below, where the line
// arrived in one read and the slice returned is bufio's own, and then only if
// the line had one at all: a stream cut mid-line has none to return. An
// accumulated line drops it, as does a truncated line, whose terminator sits in
// the discarded overflow. Nothing needs it either way, since Next trims
// surrounding space before decoding, which removes it along with a stray \r from
// a Windows-written stream, and json.Unmarshal ignores trailing whitespace
// regardless.
//
// The returned slice is valid only until the next call: it is bufio's buffer on
// the fast path and the Reader's scratch otherwise, and both are overwritten by
// the read after it. That is safe because Next is the only caller and decodes
// before reading again, and because every field reachable from an Event is a
// string, a number, or a pointer to more of the same, all of which
// json.Unmarshal copies out. A []byte or json.RawMessage field, or any type with
// an UnmarshalJSON that keeps its argument, would alias this buffer instead, and
// must not be added to Event without copying.
//
// bufio.Scanner is deliberately not used: it fails the whole stream with
// ErrTooLong on a single over-long line, which would drop every later event
// including the result.
func (r *Reader) readLine() (line []byte, truncated bool, err error) {
	buf := r.buf[:0]
	for {
		// err is the named result rather than a per-iteration variable. Both forms
		// behave identically today, since every return names it explicitly; this
		// one keeps a later bare return from producing a nil error.
		var chunk []byte
		chunk, err = r.br.ReadSlice('\n')
		// ErrBufferFull means the delimiter was not reached; keep accumulating.
		more := errors.Is(err, bufio.ErrBufferFull)
		payload := len(chunk)
		if payload > 0 && chunk[payload-1] == '\n' {
			payload--
		}
		switch room := maxLineBytes - len(buf); {
		case payload > room:
			// Fill the cap exactly with the head of the chunk that crosses it and
			// discard the rest of the line. room cannot go negative and so needs no
			// guard, because neither arm that grows buf can take it past the cap:
			// the one below runs only when payload fits in the room left, and this
			// one appends precisely the room left, landing on maxLineBytes exactly
			// and leaving every later pass through here with room 0. How much of the
			// crossing chunk lands here depends on the reader's chunk size, and is
			// zero whenever those divide the cap; either way what is retained is
			// dropped by Next rather than decoded.
			truncated = true
			buf = append(buf, chunk[:room]...)
		case len(buf) == 0 && !more:
			// The whole line came back in one read and nothing has been retained, so
			// there is nothing to accumulate it with.
			return chunk, false, err
		default:
			// Without the terminator: the cap is on payload, Next trims before
			// decoding, and keeping it would push the retained line one byte past
			// maxLineBytes, forcing a regrow of the whole buffer for a line whose
			// payload happens to end on a read boundary.
			buf = append(buf, chunk[:payload]...)
		}
		if more {
			continue
		}
		if truncated {
			// Corruption must not pin the scratch for the rest of the run; see buf.
			r.buf = nil
		} else {
			r.buf = buf
		}
		return buf, truncated, err
	}
}
