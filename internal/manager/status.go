package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

// maxReadBytes caps how much of a job's out/err file is read into memory, so a
// runaway agy emitting huge output cannot OOM the server. Reviews are text and
// far smaller than this; anything larger is truncated.
const maxReadBytes = 32 << 20 // 32 MiB

// errTailBytes is how much of the trailing stderr a failed job reports. The tail
// (not the head) is what matters: the final lines carry the actual error.
const errTailBytes = 2000

// Job states reported by Status. These are shared with StartJob and the gate
// watchdog so the producer and consumer of a job's state cannot drift apart.
const (
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Status is the observable state of a job.
type Status struct {
	State          string // running | done | failed | cancelled
	Elapsed        time.Duration
	Result         string // present when done: the assistant's response
	Error          string // present when failed: agy's own message, or a stderr tail + exit code
	ConversationID string
	// Partial marks a done job whose result was reconstructed from the streamed
	// text because agy never emitted a terminal result event (it was killed, or
	// the supervisor died mid-stream). The text may be truncated or may include
	// intermediate turns, so a caller should not treat it as the final answer.
	Partial bool
	// NumTurns and Usage are agy's own accounting, present only once a terminal
	// result event has been recorded.
	NumTurns int
	Usage    *streamjson.Usage
	// StepType names the stream step the job is on, for progress reporting. It
	// is a hint: empty until the first step arrives, and stale by up to one poll.
	StepType string
}

// Status derives a job's status from the on-disk store.
func (m *Manager) Status(id string) (Status, error) {
	meta, err := m.store.Load(id)
	if err != nil {
		return Status{}, err
	}
	dir, err := m.store.Dir(id)
	if err != nil {
		return Status{}, err
	}
	st := Status{
		Elapsed:        time.Since(meta.StartedAt),
		ConversationID: meta.ConversationID,
	}
	// The progress file is what makes a running job's conversation id readable:
	// the supervisor writes it as soon as agy's init event names the
	// conversation, which is well before the run produces an answer. meta wins
	// when set, since an explicit continuation already knows its conversation.
	if prog, ok := jobstore.ReadProgressDir(dir); ok {
		if st.ConversationID == "" {
			st.ConversationID = prog.ConversationID
		}
		st.StepType = prog.StepType
	}

	if code, ok := m.store.ExitCode(id); ok {
		return m.statusFromExitCode(dir, meta, st, code), nil
	}

	// No sentinel: decide running vs interrupted.
	if m.processAlive(meta) {
		st.State = StateRunning
		return st, nil
	}
	// The supervisor may have written the sentinel and exited between the two
	// checks above; re-read once so a job that just finished normally is not
	// misreported as interrupted.
	if code, ok := m.store.ExitCode(id); ok {
		return m.statusFromExitCode(dir, meta, st, code), nil
	}
	// Process is gone without a sentinel. The job is terminal (no supervisor is
	// left to write one), so freeze elapsed at the best available end time before
	// classifying the outcome, so a recovered job's elapsed does not keep growing.
	st.Elapsed = m.frozenElapsed(meta, st.Elapsed)
	return recoverInterrupted(dir, st), nil
}

// recoverInterrupted classifies a job whose supervisor vanished without writing
// the exit-code sentinel (killed by a reboot, say).
//
// A terminal result payload can still be present: the supervisor writes it
// before the sentinel, so a supervisor that died between those two writes left a
// complete and trustworthy result. Only when there is no payload does the
// streamed text become the best available answer, and then it is partial.
func recoverInterrupted(dir string, st Status) Status {
	if res, ok := readResultPayload(dir); ok {
		return applyResult(st, res)
	}
	out, rerr := readFile(jobstore.OutPath(dir))
	switch {
	case rerr != nil:
		st.State = StateFailed
		st.Error = fmt.Sprintf("job process exited and its output could not be read: %v", rerr)
	case out != "":
		st.State = StateDone
		st.Result = out
		st.Partial = true
	default:
		st.State = StateFailed
		st.Error = "job process exited without writing a result (interrupted)"
	}
	return st
}

// statusFromExitCode fills st from a recorded exit-code sentinel.
//
// agy's own result payload, when present, outranks the exit code for describing
// the outcome: agy reports failures it survives (an unresolvable model, its own
// print-timeout) in band with a specific message, where the exit code is only
// ever 1. The code still owns every case where no payload could be written,
// which is exactly the terminations agy-mcp itself performs.
func (m *Manager) statusFromExitCode(dir string, meta jobstore.Meta, st Status, code int) Status {
	// The job is terminal, so freeze Elapsed at the completion time rather than
	// letting it grow forever as time.Since(StartedAt).
	st.Elapsed = m.frozenElapsed(meta, st.Elapsed)
	res, hasResult := readResultPayload(dir)
	switch code {
	case 0:
		if !hasResult {
			// A clean exit whose terminal event never arrived. Report the streamed
			// text as a partial result rather than asserting a complete empty answer.
			out, err := readFile(jobstore.OutPath(dir))
			if err != nil {
				st.State = StateFailed
				st.Error = fmt.Sprintf("job completed but its output could not be read: %v", err)
				return st
			}
			st.State = StateDone
			st.Result = out
			// A run that produced an event stream but no terminal payload really was
			// cut short, so its text is partial. A job written before that flag
			// existed has a complete plain-text out and no payload to find, so
			// calling it partial would tell the caller a finished answer may be
			// truncated for the whole TTL after an upgrade.
			st.Partial = streamJSONRun(dir, meta)
			return st
		}
		return applyResult(st, res)
	case jobstore.ExitSIGTERM, jobstore.ExitSIGINT:
		st.State = StateCancelled
		st = carryStreamedText(dir, st, hasResult, res)
	case jobstore.ExitTimeout:
		st.State = StateFailed
		st.Error = "job exceeded its timeout and was terminated"
		st = carryStreamedText(dir, st, hasResult, res)
	case jobstore.ExitSpawnFail:
		// 127 is written both when the supervisor could not exec agy and when agy
		// itself exits 127, so name both causes rather than asserting one, and keep
		// any stderr (a true spawn failure has none; a genuine agy 127 does).
		st.State = StateFailed
		st.Error = spawnFailMessage(dir)
	default:
		st.State = StateFailed
		if hasResult && res.Error != "" {
			st.Error = res.Error
		} else {
			st.Error = errorSummary(dir, code)
		}
		// Any other non-zero exit is a run cut short just as much as a timeout is:
		// a crash, an OOM kill, agy exiting 1. The streamed text is the only answer
		// those will ever have, so treat them the same rather than giving a
		// timed-out run its partial answer and a crashed one nothing.
		st = carryStreamedText(dir, st, hasResult, res)
	}
	// A cancelled or timed-out run still has a conversation worth continuing, so
	// carry the id (and the accounting) from any payload that did get written.
	if hasResult {
		st = carryResultMetadata(st, res)
	}
	return st
}

// applyResult fills st from a terminal result payload, which is the complete and
// authoritative description of how the run ended.
func applyResult(st Status, res streamjson.Result) Status {
	st = carryResultMetadata(st, res)
	if res.Status == streamjson.StatusError {
		st.State = StateFailed
		st.Error = res.Error
		if st.Error == "" {
			// agy said it failed but gave no reason. Say that, rather than
			// reporting a failure with a blank cause.
			st.Error = "agy reported an error without a message"
		}
		return st
	}
	// A named status this build has never heard of. agyver sets a version floor
	// and no ceiling, so a newer agy may report an outcome such as CANCELLED or
	// MAX_TURNS; deriving success from "not ERROR" would hand those back as clean,
	// complete answers. Report the status, but keep any response it carried rather
	// than discarding recoverable work, and mark it partial since this build
	// cannot vouch for it being the final answer.
	//
	// An ABSENT status is not in that class: the field is omitempty, so its
	// absence is a shape the wire format declares legal, and an older reading
	// treated it as success. Keep doing so.
	if res.Status != "" && res.Status != streamjson.StatusSuccess {
		st.State = StateFailed
		st.Error = fmt.Sprintf("agy reported an unrecognized result status %q", res.Status)
		if res.Response != "" {
			st.Result = strings.TrimRight(res.Response, "\n")
			st.Partial = true
		}
		return st
	}
	st.State = StateDone
	// Match readFile's trimming so a result reads identically whether it came
	// from the payload or from the streamed fallback.
	st.Result = strings.TrimRight(res.Response, "\n")
	return st
}

// streamJSONRun reports whether a job actually produced an agy event stream. It
// is how a job dir written by an older agy-mcp is told apart from one this build
// produced, which matters wherever a missing result payload could mean either
// "the run was cut short" or "this build never wrote one".
//
// Two independent signals, because neither alone is sound:
//
//   - The persisted args are what this build asked for, but the supervisor
//     appends the flag when a foreign build's args lack it (ensureStreamJSON)
//     and deliberately does not write that back, since rewriting meta.json would
//     race the manager. So args can say "no stream" for a run that streamed.
//   - progress.json is evidence the supervisor itself left behind, written when
//     agy's init event arrived, so only a stream-json run has one. But a run
//     that died before init has none.
//
// Either signal is enough. Only a job with neither, which is exactly a job an
// older build wrote, reads as legacy.
func streamJSONRun(dir string, meta jobstore.Meta) bool {
	if slices.ContainsFunc(meta.Args, func(a string) bool {
		return a == outputFormatFlag || strings.HasPrefix(a, outputFormatFlag+"=")
	}) {
		return true
	}
	_, ok := jobstore.ReadProgressDir(dir)
	return ok
}

// carryStreamedText attaches the assistant's answer to a run that agy-mcp cut
// short: the terminal payload when agy managed to emit one, otherwise whatever
// text had streamed, marked partial.
//
// A payload can exist even here. agy prints its result and then hangs, or is
// cancelled a moment later, so the run is killed with a complete answer already
// on disk. That answer outranks the streamed text and is not partial; preferring
// the stream would discard the very thing the caller asked for. Only when there
// is no payload is the streamed text the best there will ever be.
//
// A read failure is not surfaced as an error because the job's state is already
// decided by its exit code; the absent text is simply not reported.
func carryStreamedText(dir string, st Status, hasResult bool, res streamjson.Result) Status {
	if hasResult && res.Response != "" {
		st.Result = strings.TrimRight(res.Response, "\n")
		return st
	}
	out, err := readFile(jobstore.OutPath(dir))
	if err != nil || out == "" {
		return st
	}
	st.Result = out
	st.Partial = true
	return st
}

// carryResultMetadata copies the fields that are worth reporting regardless of
// how the run ended: the conversation to continue, and agy's own accounting.
func carryResultMetadata(st Status, res streamjson.Result) Status {
	if st.ConversationID == "" {
		st.ConversationID = res.ConversationID
	}
	st.NumTurns = res.NumTurns
	st.Usage = res.Usage
	return st
}

// readResultPayload reads a job's terminal result event. ok is false when the
// run never reached one, which is what marks its captured output partial.
//
// A file that exists but cannot be read or decoded is also reported absent, but
// loudly: the supervisor wrote it, so the cause is corruption or a permission
// problem on disk, and silently calling a complete answer partial would hide
// that. Reporting the streamed text as partial is still the right outcome; it
// just must not be silent.
func readResultPayload(dir string) (streamjson.Result, bool) {
	b, err := jobstore.ReadResultDir(dir)
	if err != nil {
		log.Printf("job dir %s: result payload could not be read: %v", dir, err)
		return streamjson.Result{}, false
	}
	if b == nil {
		return streamjson.Result{}, false
	}
	var res streamjson.Result
	if err := json.Unmarshal(b, &res); err != nil {
		log.Printf("job dir %s: result payload could not be decoded: %v", dir, err)
		return streamjson.Result{}, false
	}
	return res, true
}

// State returns just the job's state, without paying to read its (potentially
// large) out file when the state can be decided from the exit-code sentinel
// alone. agy_cancel uses it: every actual cancel leaves a non-zero sentinel
// (SIGTERM/SIGINT, or a timeout/spawn-fail kill), so the common path reads no
// out/err at all. State never disagrees with Status: the one terminal case
// whose done-vs-failed split depends on out readability (a clean exit, code 0)
// is not fast-pathed but deferred to Status, as is the running/recovery path.
func (m *Manager) State(id string) (string, error) {
	// Fast path: a terminal sentinel other than a clean exit decides the state
	// from the code alone. A clean exit (0) is excluded because Status downgrades
	// a successful-but-unreadable out to failed, a distinction that requires the
	// read State is trying to avoid.
	if code, ok := m.store.ExitCode(id); ok && code != 0 {
		return stateForCode(code), nil
	}
	// Clean exit, or no sentinel yet: defer to Status, which reads the out file to
	// tell a clean/recovered result (done) from an unreadable or absent one
	// (failed) and handles the running and post-exit race exactly as the poller
	// sees it. Deferring here keeps State and Status from ever diverging.
	st, err := m.Status(id)
	if err != nil {
		return "", err
	}
	return st.State, nil
}

// stateForCode maps a terminal exit-code sentinel to a job state. It is the
// shared source of truth for the code->state mapping; State uses it for the
// non-zero terminal codes (a clean exit's done-vs-failed split also depends on
// out readability, so State handles 0 via Status rather than this mapping).
func stateForCode(code int) string {
	switch code {
	case 0:
		return StateDone
	case jobstore.ExitSIGTERM, jobstore.ExitSIGINT:
		return StateCancelled
	default: // timeout, spawn-fail, or any other nonzero code
		return StateFailed
	}
}

// frozenElapsed returns a terminal job's run duration measured to its recorded
// completion time (see jobstore.CompletedAt), so a long-finished job does not
// report an ever-growing elapsed. It falls back to the passed-in running
// duration (time.Since(StartedAt)) only when no completion time exists at all;
// if the completion time implausibly precedes StartedAt (clock skew) it clamps
// to 0, because the job is terminal and elapsed must stay frozen rather than
// resume growing.
func (m *Manager) frozenElapsed(meta jobstore.Meta, running time.Duration) time.Duration {
	end, ok := m.store.CompletedAt(meta.ID)
	if !ok {
		return running
	}
	if d := end.Sub(meta.StartedAt); d >= 0 {
		return d
	}
	return 0
}

// readFile returns the file's contents (trailing newline trimmed), capped at
// maxReadBytes. A missing file yields "" with no error: a job may legitimately
// have produced no output. Any other error is returned so callers can tell an
// unreadable file (report a failure) from an empty one (a clean empty result).
func readFile(p string) (string, error) {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxReadBytes))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// tailFile returns the last n bytes of the file at path. Unlike a LimitReader
// from offset 0 (which keeps the FIRST n bytes), it seeks to the end, so the
// tail is the real end of the stream even when the file is far larger than n.
// A missing file yields "" with no error.
func tailFile(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if size := info.Size(); size > n {
		start = size - n
	}
	buf := make([]byte, info.Size()-start)
	// A terminal job's file is normally static, but guard the TOCTOU window: if
	// the file shrank between Stat and ReadAt, ReadAt fills fewer than len(buf)
	// bytes and returns io.EOF. Slice to what was actually read so the tail is not
	// padded with NUL bytes from the unfilled allocation.
	read, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(buf[:read]), nil
}

// cleanTail returns the trailing stderr of a terminal job (the last errTailBytes,
// trailing newline trimmed and starting on a valid UTF-8 boundary), or "" when
// there is none.
func cleanTail(dir string) (string, error) {
	tail, err := tailFile(jobstore.ErrPath(dir), errTailBytes)
	if err != nil {
		return "", err
	}
	tail = strings.TrimRight(tail, "\n")
	// tailFile may have started mid-rune; advance to a valid UTF-8 boundary so a
	// multi-byte rune is not split.
	for tail != "" && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail, nil
}

func errorSummary(dir string, code int) string {
	tail, err := cleanTail(dir)
	if err != nil {
		// The stderr file exists but cannot be read; say so rather than emitting a
		// bare "exit N:" that looks like there was no error output.
		return fmt.Sprintf("exit %d: <stderr unavailable: %v>", code, err)
	}
	return strings.TrimSpace("exit " + strconv.Itoa(code) + ": " + tail)
}

// spawnFailMessage explains a 127 exit. The supervisor writes 127 both when it
// could not exec agy (the intended meaning) and when agy itself exits 127, so it
// names both causes and appends any stderr instead of masking it.
func spawnFailMessage(dir string) string {
	msg := "agy exited 127: the supervisor could not exec the agy binary (check the configured agy path), or agy itself exited 127"
	if tail, err := cleanTail(dir); err == nil && tail != "" {
		msg += "; stderr: " + tail
	}
	return msg
}
