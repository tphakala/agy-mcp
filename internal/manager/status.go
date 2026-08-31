package manager

import (
	"bytes"
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
	"unicode"
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

// Failure reasons reported in Status.FailureReason. They classify a StateFailed
// job so a caller can branch on the cause without scraping the free-text Error:
// above all, tell a transient wall it can wait out (ReasonQuotaExhausted) from a
// hard error. The set is small, stable and closed; an outcome that fits none of
// the named reasons is ReasonUnknown rather than a new value, so a caller's
// switch never has to grow to stay correct.
//
// A reason is set only alongside StateFailed. A cancelled job needs none (its
// state already is the reason), and a running or done job has no failure to
// name.
const (
	ReasonQuotaExhausted = "quota_exhausted" // agy hit a provider quota or rate-limit wall; transient
	ReasonTimeout        = "timeout"         // agy-mcp killed the run for exceeding its timeout
	ReasonSpawnFailed    = "spawn_failed"    // the agy binary could not be started, or agy itself exited 127 (one exit sentinel covers both)
	ReasonAgyError       = "agy_error"       // agy itself reported an error, exited non-zero, or returned an indeterminate result
	ReasonInterrupted    = "interrupted"     // the job process vanished without writing a result
	ReasonUnknown        = "unknown"         // a failure that fits none of the above (e.g. the output could not be read)
)

// Status is the observable state of a job.
type Status struct {
	State   string // running | done | failed | cancelled
	Elapsed time.Duration
	// Result is the assistant's response. Every TERMINAL state whose text can be
	// RECOVERED carries one, not done alone: a cancelled, timed-out or crashed
	// run carries whatever it managed to say, so that work is offered back rather
	// than discarded. Recoverable is the operative word, since text can exist on
	// disk and still not be readable: that leaves this empty, reported as an Error
	// where the read was the only possible source of an answer
	// (cleanExitWithoutPayload, recoverInterrupted) and passed over silently where
	// the state was already decided without it (carryText). Consult Partial before
	// treating it as final.
	//
	// A running job deliberately reports none, even once it has streamed text.
	// Status returns above without reading the out file while the job is live,
	// which is what makes polling one cheap (the agy_status tool description
	// promises exactly that); reading per tick would re-read a growing file, and
	// mid-stream text is not an answer. Collect a result once the state is
	// terminal.
	Result string
	Error  string // present when failed: agy's own message, or a stderr tail + exit code
	// FailureReason classifies a StateFailed job into one of the stable Reason
	// constants, so a caller can branch on the cause (most usefully, tell a
	// transient ReasonQuotaExhausted wall from a hard error) without scraping
	// Error. It is set only when State is StateFailed; a cancelled, running or
	// done job leaves it empty. Error still carries the human-readable detail,
	// including a quota wall's reset time, which this field deliberately does not
	// parse out.
	FailureReason  string
	ConversationID string
	// Model is the model id agy-mcp resolved for this run and persisted to meta:
	// the request's model, or AGY_MCP_DEFAULT_MODEL when none was given, reduced to
	// the id column (see modelID). Empty when neither was set, meaning the server
	// pinned no model and agy ran on its own default (which this build cannot name).
	Model string
	// Partial marks a Result this build cannot vouch for as the complete final
	// answer. It follows from where the text came from, not from State:
	//
	//   - Reconstructed from the event stream, because agy never emitted a
	//     terminal result event (it was killed, timed out, or its supervisor died
	//     mid-stream). The text may be truncated or hold intermediate turns.
	//   - Taken from a terminal event agy did emit but did not mark SUCCESS: an
	//     ERROR, an outcome this build does not recognize, or none at all. The
	//     text is whatever the run had produced when it stopped.
	//
	// A response agy marked SUCCESS is never partial, even when the job then
	// ended as cancelled or failed: the answer was already complete when that
	// happened.
	Partial bool
	// NumTurns and Usage are agy's own accounting, present only once a terminal
	// result event has been recorded.
	NumTurns int
	Usage    *streamjson.Usage
	// StepType names the stream step the job is on, for progress reporting. It
	// is a hint: empty until the first step arrives, and stale by up to one poll.
	// It is read from progress.json before the exit-code sentinel is consulted,
	// so a terminal job keeps the last step it recorded rather than clearing it;
	// only a running job's is current.
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
		Model:          meta.Model,
	}
	// The progress file is what makes a running job's conversation id readable:
	// the supervisor records it as soon as agy's init event names the
	// conversation, which is well before the run produces an answer. meta wins
	// when set, since an explicit continuation already knows its conversation.
	//
	// The file's presence proves only that a supervisor started the job, not
	// that agy has spoken: it is created empty before agy is even exec'd (see
	// markStreamJSON, which is why streamJSONRun can read it as a marker). So
	// both fields below are still absent until the stream supplies them, and
	// neither may be read as "the run has begun reporting".
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
// before the sentinel, so a supervisor that died between those two writes left
// agy's own account of the run, which outranks anything reconstructed here. It
// is applied exactly as it would be on the sentinel path, so whether it can be
// vouched for still depends on the status agy gave it, and its text still falls
// back to the stream when the payload carried none.
//
// Only when there is no payload at all does this function decide for itself,
// and then the streamed text is partial. It reads out here rather than through
// carryText because it needs the read ERROR: an out file that exists and cannot
// be read is a failure to report, not an absent answer to pass over.
func recoverInterrupted(dir string, st Status) Status {
	if res, ok := readResultPayload(dir); ok {
		return applyResult(dir, st, res)
	}
	out, rerr := readFile(jobstore.OutPath(dir))
	switch {
	case rerr != nil:
		st.State = StateFailed
		st.Error = fmt.Sprintf("job process exited and its output could not be read: %v", rerr)
		st.FailureReason = ReasonUnknown
	case out != "":
		st.State = StateDone
		st.Result = out
		st.Partial = true
	default:
		st.State = StateFailed
		st.Error = "job process exited without writing a result (interrupted)"
		st.FailureReason = ReasonInterrupted
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
//
// Every non-zero code shares one epilogue rather than repeating it per branch.
// The branches differ only in the state and the explanation they choose; what
// happens to the answer and the accounting afterwards is the same for all of
// them, and this file has regressed three times because that was not true and a
// fix applied to one branch left the identical case open in its sibling.
func (m *Manager) statusFromExitCode(dir string, meta jobstore.Meta, st Status, code int) Status {
	// The job is terminal, so freeze Elapsed at the completion time rather than
	// letting it grow forever as time.Since(StartedAt).
	st.Elapsed = m.frozenElapsed(meta, st.Elapsed)
	res, hasResult := readResultPayload(dir)
	switch code {
	case 0:
		if !hasResult {
			return cleanExitWithoutPayload(dir, meta, st)
		}
		return applyResult(dir, st, res)
	case jobstore.ExitSIGTERM, jobstore.ExitSIGINT:
		st.State = StateCancelled
	case jobstore.ExitTimeout:
		st.State = StateFailed
		st.Error = "job exceeded its timeout and was terminated"
		st.FailureReason = ReasonTimeout
	case jobstore.ExitSpawnFail:
		// 127 is written both when the supervisor could not exec agy and when agy
		// itself exits 127, so name both causes rather than asserting one, and keep
		// any stderr (a true spawn failure has none; a genuine agy 127 does).
		st.State = StateFailed
		st.Error = spawnFailMessage(dir)
		st.FailureReason = ReasonSpawnFailed
	default:
		st.State = StateFailed
		if hasResult && res.Error != "" {
			st.Error = res.Error
		} else {
			st.Error = errorSummary(dir, code)
		}
		// A non-zero exit's message can still be a quota wall (agy usually reports
		// one in its payload, but may exit non-zero with it only on stderr), so
		// classify the message rather than flatly calling every crash agy_error.
		st.FailureReason = classifyAgyError(st.Error)
	}
	// Every non-zero exit is a run cut short: a cancel, a timeout, a crash, an
	// OOM kill, agy exiting 1, and a 127 that is a real agy exit rather than a
	// failed exec. Whatever text they produced is the only answer they will ever
	// have, so they all carry it rather than giving a timed-out run its partial
	// answer and a crashed or 127'd one nothing.
	st = carryText(dir, st, hasResult, res)
	// A cancelled or timed-out run still has a conversation worth continuing, so
	// carry the id (and the accounting) from any payload that did get written.
	if hasResult {
		st = carryResultMetadata(st, res)
	}
	return st
}

// cleanExitWithoutPayload classifies a job that exited 0 without ever reaching a
// terminal result event. It reports the streamed text rather than asserting a
// complete empty answer, which is what a bare "done" with nothing in it would
// mean to a caller.
func cleanExitWithoutPayload(dir string, meta jobstore.Meta, st Status) Status {
	out, err := readFile(jobstore.OutPath(dir))
	if err != nil {
		st.State = StateFailed
		st.Error = fmt.Sprintf("job completed but its output could not be read: %v", err)
		st.FailureReason = ReasonUnknown
		return st
	}
	st.State = StateDone
	st.Result = out
	// A run that produced an event stream but no terminal payload really was cut
	// short, so its text is partial. A job written before that flag existed has a
	// complete plain-text out and no payload to find, so calling it partial would
	// tell the caller a finished answer may be truncated for the whole TTL after
	// an upgrade.
	st.Partial = streamJSONRun(dir, meta)
	return st
}

// applyResult fills st from a terminal result payload, which is agy's own
// description of how the run ended.
//
// It decides the state, the explanation, and the accounting worth reporting
// whatever the outcome (via carryResultMetadata). The answer itself, and
// whether this build can vouch for it, are left to carryText so that a payload
// is read the same way here as on every other terminal path.
func applyResult(dir string, st Status, res streamjson.Result) Status {
	st = carryResultMetadata(st, res)
	switch {
	case res.Status == streamjson.StatusError:
		st.State = StateFailed
		// Classify off agy's own message: this is where a quota wall lands, and
		// telling it apart from a generic error is the whole point of the field.
		st.FailureReason = classifyAgyError(res.Error)
		st.Error = res.Error
		if st.Error == "" {
			// agy said it failed but gave no reason. Say that, rather than
			// reporting a failure with a blank cause.
			st.Error = "agy reported an error without a message"
		}
	case res.Status == streamjson.StatusSuccess:
		st.State = StateDone
	// An ABSENT status is only treated as success when the payload actually
	// carries an answer. `omitempty` is agy-mcp's own struct tag, so it is
	// evidence about this decoder, not a statement about agy's wire format;
	// reading absence alone as success turns a payload whose recognized fields
	// are all missing (a future agy that renames or restructures them) into a
	// completed, empty, non-partial answer. A response with no status is
	// recoverable; neither is indeterminate, and indeterminate is a failure.
	//
	// Recoverable is not verified: carryText flags it, because a future agy that
	// renames or restructures the status field while keeping a response would
	// otherwise have a run cut short by MAX_TURNS reported as a clean, complete
	// answer, which is the exact failure this branch exists to prevent.
	case res.Status == "" && res.Response != "":
		st.State = StateDone
	case res.Status == "":
		st.State = StateFailed
		st.Error = "agy's result payload carried no status and no response"
		st.FailureReason = ReasonAgyError
	// A named status this build has never heard of. agyver sets a version floor
	// and no ceiling, so a newer agy may report an outcome such as CANCELLED or
	// MAX_TURNS; deriving success from "not ERROR" would hand those back as
	// clean, complete answers. Report the status, and let carryText keep any
	// response it carried rather than discarding recoverable work.
	default:
		st.State = StateFailed
		st.Error = fmt.Sprintf("agy reported an unrecognized result status %q", res.Status)
		st.FailureReason = ReasonAgyError
	}
	return carryText(dir, st, true, res)
}

// streamJSONRun reports whether a job dir carries evidence that this build, or
// any build that drives agy through --output-format, produced it. It is how
// such a dir is told apart from one an older agy-mcp wrote, which matters
// wherever a missing result payload could mean either "the run was cut short"
// or "this build never wrote one".
//
// Read the name as "not a legacy job dir" rather than as a claim about the
// stream-json format specifically. The args signal below matches ANY output
// format, so a job that asked for --output-format=json also answers true here.
// That is deliberate and conservative for this caller, which uses the answer
// only to decide whether an absent payload means the run was cut short: such a
// job produces no decodable stream either, so treating its output as partial is
// correct. It would NOT be a sound basis for a caller asking the narrower
// question, and #102 tracks tightening it.
//
// Three independent signals, because no one of them is sound alone. Any of them
// is enough; only a job with none, which is what an older build wrote, reads as
// legacy.
//
//   - The persisted args are what this build asked for, but the supervisor
//     appends the flag when a foreign build's args lack it (ensureStreamJSON)
//     and deliberately does not write that back, since rewriting meta.json would
//     race the manager. So args can say "no stream" for a run that streamed.
//   - progress.json is evidence the supervisor itself left behind. It writes the
//     file before agy is even started (markStreamJSON), so every run it drives
//     has one, including a run whose agy emitted nothing at all. That is the
//     half the args cannot cover: an upgraded job whose agy said nothing has no
//     flag in its args either, and once read as legacy its empty output would be
//     reported as a complete answer rather than a cut-short one.
//   - result.json existing at all. Only the supervisor writes it, and only from
//     a decoded terminal event, so its presence proves a stream was decoded even
//     when its contents can no longer be read.
//
// That last signal is why this asks the filesystem rather than reusing the
// caller's decoded payload. The two callers reach here precisely when
// readResultPayload said "no payload", and that answer covers a file that is
// present but corrupt or unreadable as well as one that was never written.
// Without this check such a job has only the other two signals, so an upgraded
// one reads as legacy and its streamed text is reported as a complete, verified
// answer. Both remaining signals fail open in the same direction, which is the
// direction that misreports.
func streamJSONRun(dir string, meta jobstore.Meta) bool {
	if argsSelectOutputFormat(meta.Args) {
		return true
	}
	if _, ok := jobstore.ReadProgressDir(dir); ok {
		return true
	}
	_, err := os.Stat(jobstore.ResultPath(dir))
	return err == nil
}

// argsSelectOutputFormat reports whether persisted args name an output format,
// in either spelling the Go flag package accepts. It exists as a named function
// rather than inline so the test that validates the terminal-contract table can
// ask exactly the question streamJSONRun asks: a second copy of the predicate
// would let a row spelled with the inline form be classified as a legacy job
// dir by the validator and as a stream-json run by the code, which is the one
// disagreement a self-checking table cannot catch.
func argsSelectOutputFormat(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return a == outputFormatFlag || strings.HasPrefix(a, outputFormatFlag+"=")
	})
}

// carryText attaches the answer a run produced and records whether this build
// can vouch for it as the final one.
//
// It is where every path that HAS a payload to consider makes both decisions,
// which is all of statusFromExitCode and applyResult. Two terminal paths decide
// for themselves and are the exceptions to look for before assuming a change
// here reaches everything:
//
//   - cleanExitWithoutPayload, whose text is always streamed and which carries
//     the one deliberate exception to "streamed text is partial" (a job an older
//     build wrote, whose plain-text out really is complete).
//   - recoverInterrupted's no-payload branch, which reads out itself because it
//     must tell an unreadable file (a failure) from an empty one, a distinction
//     this function deliberately discards.
//
// There are exactly two sources of text, and which one supplied it is the whole
// of the Partial decision:
//
//   - agy's terminal payload. It is authoritative, so a response agy itself
//     marked SUCCESS is the complete answer even when the job then ended badly:
//     agy prints its result and hangs, or is cancelled a moment later, so the
//     run is killed with a finished answer already on disk, and preferring the
//     stream there would discard the very thing the caller asked for. Any other
//     status (ERROR, an outcome this build has never heard of, or none at all)
//     is agy declining to vouch for the text, so it is handed back and flagged
//     rather than discarded.
//   - the streamed out file, reached when there is no payload or the payload
//     carried no text. It is reconstructed from a stream that stopped, so it may
//     be truncated or hold intermediate turns, and is partial.
//
// Note that the second case can follow a SUCCESS payload: agy vouched for the
// run but its response was empty, so the text handed back is the stream's, not
// agy's, and it is flagged accordingly. "A response agy marked SUCCESS is never
// partial" is a statement about agy's RESPONSE, not about every result reported
// for a run agy marked successful.
//
// Provenance, not state, is what decides Partial. State cannot tell the two
// apart: a cancelled run holds a complete answer in the first case and a
// truncated one in the second, and a done run holds an unverifiable one
// whenever the payload's status is absent.
//
// A read failure is not surfaced as an error because the job's state is already
// decided by its exit code or its payload; the absent text is simply not
// reported.
func carryText(dir string, st Status, hasResult bool, res streamjson.Result) Status {
	if hasResult && res.Response != "" {
		// Match readFile's trimming so a result reads identically whether it came
		// from the payload or from the streamed fallback.
		st.Result = strings.TrimRight(res.Response, "\n")
		// Raised, never lowered. Partial is a pessimistic flag: the only safe
		// operation on it is to set it. No caller sets it before calling here
		// today, so this is currently the same as an assignment, but a caller
		// that learns the text is untrustworthy for its own reason (skipped
		// lines in the stream, say) must not have that silently cleared by a
		// payload agy happened to mark SUCCESS.
		st.Partial = st.Partial || res.Status != streamjson.StatusSuccess
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
//
// The read is pre-sized from the file's own size, so a large out file is not
// rebuilt by the grow-and-copy an unsized io.ReadAll performs. What pays for
// this is the fallback paths rather than every terminal status: a run whose
// terminal payload carried a response is answered from that and never opens out
// at all (see carryText). The callers that do open it are the runs cut short,
// the recovered ones, and a legacy job dir, and those are polled like any other.
// The size is only a hint: the LimitReader still owns the cap (so an out over it
// is truncated, not read whole), and the buffer still grows on its own if the
// file changed since the Stat.
func readFile(p string) (string, error) {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var buf bytes.Buffer
	// Pre-size from the file's own size, plus bytes.MinRead of headroom. The
	// headroom is what keeps ReadFrom's last pass from reallocating: it grows by
	// MinRead before every read, which is a free reslice while that much spare
	// capacity remains and a full grow-and-copy once it is not. That applies to
	// an oversized file too, so this deliberately reserves MinRead more than the
	// LimitReader can ever deliver; reserving exactly maxReadBytes instead buys
	// back those 512 bytes and pays a 32 MiB copy to discover EOF (measured on a
	// file over the cap: 100.7 MB allocated per read against 33.6 MB, and 3.5ms
	// against 2.4ms). A Stat failure costs only the pre-sizing, so it is not
	// reported; a zero or negative size skips it (Grow panics on a negative
	// argument).
	if info, serr := f.Stat(); serr == nil && info.Size() > 0 {
		buf.Grow(int(min(info.Size(), maxReadBytes) + bytes.MinRead))
	}
	if _, err := buf.ReadFrom(io.LimitReader(f, maxReadBytes)); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
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

// classifyAgyError maps an error message agy produced (a terminal ERROR
// payload, or a non-zero exit's stderr tail) to a failure reason. A provider
// quota or rate-limit wall is the one transient, retryable case and is told
// apart as ReasonQuotaExhausted; everything else agy reports is ReasonAgyError.
//
// It is only ever called on the branches that would otherwise be a flat
// ReasonAgyError, so it never has to name the structural reasons (timeout,
// spawn-fail, interrupted) that their own branches already know.
func classifyAgyError(msg string) string {
	if isQuotaError(msg) {
		return ReasonQuotaExhausted
	}
	return ReasonAgyError
}

// isQuotaError reports whether an error message describes a provider quota or
// rate-limit wall: a transient condition that clears on its own, distinct from a
// hard failure. agy relays the provider's own wording (observed as "Individual
// quota reached. Please upgrade your subscription to increase your limits.
// Resets in 21m50s."), which agy-mcp does not control, so the match is a
// case-insensitive scan for the phrases that wording and the common provider
// spellings (including gRPC RESOURCE_EXHAUSTED and HTTP 429) use.
//
// The match is a substring heuristic, so it is not exact in either direction.
// The two directions are not equally safe. A provider phrasing none of these
// catch falls back to ReasonAgyError: a real quota wall then reads as a generic
// error, which still tells the truth and only loses the retryable hint. The
// dangerous direction is the reverse, a hard error whose text merely contains one
// of these words being called retryable, because the caller is then told to wait
// for a reset that never comes. The one common collision, EDQUOT's "disk quota
// exceeded", is excluded outright; other collisions (a local "resource exhausted"
// from an OOM, say) remain a small accepted risk, since agy surfaces provider
// wording far more often than raw OS errors. So this does not claim to never
// mislabel a hard error, only to bias toward under-matching and to rule out the
// one hard-error phrase seen in practice.
func isQuotaError(msg string) bool {
	l := strings.ToLower(msg)
	// "disk quota exceeded" (EDQUOT) is a hard OS storage error that contains
	// "quota" but never clears on its own, so it must never read as retryable. The
	// exclusion is scoped to the "quota" token alone, not the whole function, so a
	// message that somehow carried both "disk quota" and a genuine rate-limit
	// signal still classifies as retryable off that other signal.
	quota := strings.Contains(l, "quota") && !strings.Contains(l, "disk quota")
	return quota ||
		strings.Contains(l, "rate limit") ||
		strings.Contains(l, "rate-limit") ||
		// "http 429", not a bare "429": the status line is a strong rate-limit
		// signal, while the lone number would collide with any offset, id or count.
		strings.Contains(l, "http 429") ||
		strings.Contains(l, "resource exhausted") ||
		strings.Contains(l, "resource_exhausted") ||
		strings.Contains(l, "resourceexhausted") ||
		strings.Contains(l, "too many requests")
}

// errorSummary describes a non-zero exit that agy left no result payload for.
// The three outcomes are kept distinct because a caller reading only this string
// cannot otherwise tell them apart: stderr had something, stderr was empty, or
// stderr could not be read at all.
func errorSummary(dir string, code int) string {
	tail, err := cleanTail(dir)
	if err != nil {
		// The stderr file exists but cannot be read; say so rather than implying
		// the run produced no error output.
		return fmt.Sprintf("exit %d: <stderr unavailable: %v>", code, err)
	}
	// cleanTail strips trailing newlines but not other trailing whitespace. The
	// previous TrimSpace over the whole assembled string did strip it (the "exit"
	// prefix meant only the tail end was ever affected), so keep that here rather
	// than letting a tail of spaces through as if it were a message.
	tail = strings.TrimRightFunc(tail, unicode.IsSpace)
	if tail == "" {
		// A readable but empty stderr. Naming it beats the dangling "exit N:" that
		// trimming a colon with nothing after it used to leave behind, which read
		// as a truncated message rather than as an absence of one.
		return fmt.Sprintf("exit %d (no stderr output)", code)
	}
	return "exit " + strconv.Itoa(code) + ": " + tail
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
