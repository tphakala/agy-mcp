// Package streamjson decodes agy's `--output-format stream-json` event stream:
// newline-delimited JSON carrying an init event, per-step updates, and one
// terminal result.
package streamjson

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
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
type Init struct {
	Cwd            string   `json:"cwd,omitempty"`
	Tools          []string `json:"tools,omitempty"`
	PermissionMode string   `json:"permission_mode,omitempty"`
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

// maxLineBytes caps how much of one NDJSON line is retained. agy's init event
// embeds the full tool list and a step_update can carry a large text_delta, so
// the limit is generous; it exists only to keep a corrupt or unterminated
// stream from growing the supervisor's memory without bound. A longer line is
// truncated and reported as malformed rather than decoded from a partial
// prefix, which would be worse than dropping it.
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
}

// NewReader returns a Reader decoding r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReader(r)}
}

// Malformed returns how many lines have been skipped so far because they
// exceeded maxLineBytes or failed to decode. Blank lines are padding and are
// not counted. It is a running total, updated as the stream is consumed, so it
// is readable at any point and is final once Next has returned an error.
func (r *Reader) Malformed() int { return r.malformed }

// Next returns the next decodable event. It returns io.EOF at a clean end of
// stream and any other read error unchanged, so a stream torn off mid-run is
// reported as the failure it is rather than passed off as a finished one. Along
// the way it skips blank lines silently, and skips undecodable and over-long
// lines after counting them in Malformed.
func (r *Reader) Next() (Event, error) {
	for {
		line, truncated, err := r.readLine()
		trimmed := strings.TrimSpace(string(line))
		switch {
		case trimmed == "":
			// A blank line is padding, not corruption; do not count it.
			if err != nil {
				return Event{}, err
			}
			continue
		case truncated:
			r.malformed++
			if err != nil {
				return Event{}, err
			}
			continue
		}
		var ev Event
		if decErr := json.Unmarshal([]byte(trimmed), &ev); decErr != nil {
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

// readLine reads one newline-terminated line, growing past bufio's internal
// buffer. It returns the line with its terminator still attached whenever the
// stream supplied one, whether the line exceeded maxLineBytes (in which case the
// overflow is discarded and the retained prefix must not be decoded), and the
// terminating error if any. Two reachable paths carry no terminator: a stream that
// ends mid-line, and a truncated line, whose newline sits in the discarded
// overflow.
//
// Nothing here strips it, because nothing needs it stripped: Next trims
// surrounding space before decoding, which removes the terminator along with a
// stray \r from a Windows-written stream, and json.Unmarshal would ignore trailing
// whitespace regardless.
//
// bufio.Scanner is deliberately not used: it fails the whole stream with
// ErrTooLong on a single over-long line, which would drop every later event
// including the result.
func (r *Reader) readLine() (line []byte, truncated bool, err error) {
	var buf []byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		if room := maxLineBytes - len(buf); len(chunk) > room {
			truncated = true
			if room > 0 {
				buf = append(buf, chunk[:room]...)
			}
		} else {
			buf = append(buf, chunk...)
		}
		// ErrBufferFull means the delimiter was not reached; keep accumulating.
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf, truncated, err
	}
}
