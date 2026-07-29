// Package supervisor runs a single agy process on behalf of agy-mcp and writes
// the exit-code sentinel, so a job survives the death of the manager.
package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

// killGrace is how long the supervisor waits after SIGTERM before escalating to
// SIGKILL when terminating the agy process group on cancel or timeout. Run
// passes it to run; a test injects a smaller grace to exercise the escalation
// without a 10s wait.
const killGrace = 10 * time.Second

// fallbackTimeout bounds a job whose meta records a non-positive timeout (a
// misconfigured DefaultTimeout, or an old meta). The hard-timeout contract is
// that the run is always bounded; without this a zero timeout would leave the
// deadline disabled and cmd.Wait could block forever on a hung agy.
const fallbackTimeout = time.Hour

// drainGrace bounds how long the supervisor waits, after agy has been reaped,
// for the stdout stream to reach EOF on its own.
//
// EOF needs every holder of the write end to close it, and agy's descendants
// inherit that descriptor. A descendant that outlived the process group (it
// called setsid, or the group kill could not reach it) holds the pipe open
// indefinitely, which without this bound would stall the exit-code sentinel and
// strand the job in `running` forever. agy's own conversation-cache daemon is a
// documented example of a process that outlives a print run.
//
// It is generous because it is only ever paid in that abnormal case: with no
// lingering descendant the pipe is already closed by the time Wait returns.
const drainGrace = 5 * time.Second

// outputFormatFlag and streamJSONFormat are agy's print-mode output selector.
// They are duplicated from the manager's arg builder rather than shared: the
// supervisor's whole reason for re-checking them is that it may be reading a
// meta.json some OTHER build of agy-mcp wrote, so it cannot assume that build
// spelled them the same way it does.
const (
	outputFormatFlag = "--output-format"
	streamJSONFormat = "stream-json"
)

// ensureStreamJSON guarantees agy is asked for the event stream this supervisor
// knows how to decode, appending the flag when the job's persisted args lack it.
//
// This is the guard for an in-place binary upgrade. The supervisor is the same
// agy-mcp binary re-executed as `agy-mcp run-job <dir>`, so replacing the binary
// while a server is live pairs an OLD manager with a NEW supervisor: the manager
// writes args with no --output-format, agy prints plain text, and every line
// fails to decode, leaving an empty result the manager reports as a clean, empty
// success. Appending the flag here makes the supervisor self-sufficient instead
// of trusting args written by a build it cannot see.
//
// The args are copied rather than appended in place: they come from the caller's
// Meta and must not be mutated.
func ensureStreamJSON(args []string) []string {
	if selectsOutputFormat(args) {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, args...)
	return append(out, outputFormatFlag, streamJSONFormat)
}

// selectsOutputFormat reports whether args already choose an output format, in
// either spelling the Go flag package accepts: a separate value
// ("--output-format stream-json") or an inline one ("--output-format=json").
// Matching only the separate form would append a second flag to an inline one,
// and since the flag package takes the last occurrence that would silently
// override a format the caller chose deliberately.
func selectsOutputFormat(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return a == outputFormatFlag || strings.HasPrefix(a, outputFormatFlag+"=")
	})
}

// markStreamJSON records, before agy is even started, that this run is being
// driven through the stream-json event format.
//
// It is the durable half of ensureStreamJSON. That function may have just
// appended the flag to a COPY of args an older manager persisted, and the
// upgrade is deliberately never written back to meta.json: the manager rewrites
// meta.json right after spawning this supervisor, to record the PID it needs for
// liveness and cancel, so a second writer here would race that update and could
// leave the job untrackable. progress.json has exactly one writer, this process,
// which is why the marker goes here instead.
//
// Writing it now rather than on the first decoded event is what closes the gap.
// The manager asks "was this a stream-json run?" to tell a run that was cut
// short from a job an older build wrote, and answers it from the persisted args
// first, falling back to whether a progress file exists. An upgraded job's args
// say no; a run whose agy emitted no events at all had no progress file either,
// so it read as legacy and its empty output was reported as a complete answer
// rather than a cut-short one. A marker written at spawn time exists for every
// run this supervisor drives, whether or not agy ever says anything.
//
// The empty Progress it writes is what every reader already handles: the
// conversation id and step type are absent until the stream supplies them, which
// is exactly the state a job is in before agy starts.
//
// It is written only for a run actually being driven through stream-json.
// ensureStreamJSON leaves args alone when they already choose a format, so a
// job asking for --output-format=json is exec'd as JSON and gets no marker: a
// marker there would be a claim about a stream that is never produced. That
// costs nothing, because such a job carries a format in its args and so is
// already recognized by the args signal.
//
// The write replaces the file rather than merging into it, so it has to happen
// before the stream drain starts. After that, consumeStream owns the file and
// an overwrite here would erase the conversation id it recorded.
//
// Failure is not fatal. The marker only sharpens a heuristic that still has the
// persisted args and the result payload to fall back on, and refusing to run
// the job over it would trade a narrow misreport for a total one. It is noted
// on the captured stderr rather than swallowed; note that the note lands at the
// HEAD of that file, before agy writes a byte, so a run whose agy is loud on
// stderr pushes it outside the tail the manager reports. In practice the branch
// is close to unreachable: this write and the exit-code sentinel go through the
// same helper into the same directory, so a failure here means the sentinel
// fails too and the job is classified by the recovery path, which never
// consults the marker at all.
func markStreamJSON(jobDir string, errW io.Writer) {
	if err := jobstore.WriteProgressDir(jobDir, jobstore.Progress{UpdatedAt: time.Now().UTC()}); err != nil {
		_, _ = fmt.Fprintf(errW, "\nagy-mcp: could not record the stream-json marker: %v\n", err)
	}
}

// selectsStreamJSON reports whether args ask agy for the event stream this
// supervisor decodes, in either spelling the Go flag package accepts. It is the
// narrow question markStreamJSON needs, where selectsOutputFormat asks the wide
// one ("did the caller choose ANY format, so ensureStreamJSON must not append").
//
// The LAST occurrence decides, matching agy's own flag parsing, so a repeated
// flag is read the way agy will read it rather than the way it is written.
// ensureStreamJSON never produces a repeat, since it appends only when no
// format is present at all, but these args can come from a meta.json some other
// build wrote, which is the whole reason this supervisor re-checks them.
func selectsStreamJSON(args []string) bool {
	selected := ""
	for i, a := range args {
		switch {
		case a == outputFormatFlag && i+1 < len(args):
			selected = args[i+1]
		case strings.HasPrefix(a, outputFormatFlag+"="):
			selected = strings.TrimPrefix(a, outputFormatFlag+"=")
		}
	}
	return selected == streamJSONFormat
}

// effectiveTimeout floors a non-positive job timeout to fallbackTimeout so the
// hard timeout always fires.
func effectiveTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return fallbackTimeout
	}
	return d
}

// closed reports whether ch is already closed, without blocking.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// resolveExitCode applies the supervisor's termination overrides to the raw exit
// code derived from agy's wait status. A supervisor-initiated termination keeps
// its own meaning even when it escalated to SIGKILL: agy that ignores SIGTERM and
// is then SIGKILLed dies with signal 9 (raw 137), but the job was timed out or
// cancelled, not crashed, so it must report ExitTimeout or ExitSIGTERM rather
// than the raw signal failure. A natural exit (waitFailed false, or neither flag
// set) keeps its raw code. Timeout takes precedence; in practice only one flag is
// ever set.
func resolveExitCode(raw int, waitFailed, timedOut, cancelled bool) int {
	if !waitFailed {
		return raw
	}
	switch {
	case timedOut:
		return jobstore.ExitTimeout
	case cancelled:
		return jobstore.ExitSIGTERM
	}
	return raw
}

// Run executes the agy process described by jobDir/meta.json. It captures
// stdout to jobDir/out and stderr to jobDir/err, redirects agy stdin from
// /dev/null, and writes jobDir/exit_code on completion (including on cancel).
// Run returns an error only for setup failures, not for a non-zero agy exit.
func Run(jobDir string) error {
	return run(jobDir, killGrace, drainGrace)
}

// run is Run with an injectable SIGTERM->SIGKILL grace, so a test can exercise
// the escalation without the 10s production wait. Passing grace as a parameter
// (rather than mutating a package global) keeps the timer goroutine's read
// race-free.
func run(jobDir string, grace, drainWait time.Duration) error {
	if !proc.Supported {
		// Job supervision is implemented on Linux and macOS (process groups) and
		// Windows (Job Objects). On other platforms the proc stubs cannot terminate
		// agy, so the hard timeout would "fire" without killing anything and Run
		// would block in cmd.Wait forever. Refuse here, matching StartJob's guard on
		// the manager side.
		return proc.ErrUnsupported
	}
	m, err := jobstore.LoadDir(jobDir)
	if err != nil {
		return err
	}

	// Arm cancel detection first, before opening the job files and starting agy. A
	// cancel (SIGTERM on Linux, a sentinel file on Windows) arriving during this
	// startup window would otherwise be missed, leaving agy with nobody to
	// terminate it, the timeout unenforced, and no sentinel written. waitForCancel
	// holds an early cancel (a buffered signal, or a file that persists), so it is
	// not lost. Arming it before the job files are created also makes their
	// existence a sound readiness barrier for tests: once out/err exist, cancel is
	// already armed.
	cancel, stopCancel := waitForCancel(jobDir)
	defer stopCancel()

	// 0600: out/err capture full agy output, which often embeds source code, so
	// they must not be readable by other users on a multi-user host. os.Create would
	// use 0666 (umask-reduced), so open them explicitly owner-only instead.
	outF, err := os.OpenFile(jobstore.OutPath(jobDir), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = outF.Close() }()
	errF, err := os.OpenFile(jobstore.ErrPath(jobDir), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = errF.Close() }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { _ = devnull.Close() }()

	// Resolve the args once and reuse them, so the marker below is decided from
	// exactly what agy is being run with rather than from a second guess at it.
	args := ensureStreamJSON(m.Args)
	cmd := exec.Command(m.AgyPath, args...)
	cmd.Dir = m.Cwd
	cmd.Env = os.Environ() // agy needs HOME/PATH and its OAuth/API credentials
	cmd.Stdin = devnull
	cmd.Stderr = errF
	// agy runs with --output-format stream-json, so stdout is an event stream to
	// decode rather than text to copy. Reading it is what makes the conversation
	// id observable while the run is still going (the init event carries it) and
	// what turns an interrupted run into recoverable partial text.
	//
	// An explicit pipe, not cmd.StdoutPipe: Wait closes a StdoutPipe as soon as it
	// reaps the process, which would truncate a drain still running on its own
	// goroutine. Owning both ends means Wait touches neither, so the drain ends
	// only on a real EOF or when this function closes the read end deliberately.
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = pr.Close() }()
	cmd.Stdout = pw
	// Put agy in its own process group / job so the whole tree can be terminated
	// together on cancel or timeout.
	proc.ConfigureGroup(cmd)

	if selectsStreamJSON(args) {
		markStreamJSON(jobDir, errF)
	}

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = jobstore.WriteExitCodeDir(jobDir, jobstore.ExitSpawnFail)
		return err
	}
	// Drop the parent's write end now that the child holds its own, so the reader
	// sees EOF once agy and its descendants are gone. Keeping it open here would
	// hold the pipe open forever by ourselves.
	_ = pw.Close()
	// Capture a handle to agy's process tree so cancel/timeout can terminate it and
	// its descendants as a unit (kill -pgid on Linux, a Job Object on Windows).
	// killOnClose=true: if the supervisor itself dies unexpectedly, the Job Object
	// closing tears down agy and its descendants too (on Windows), rather than
	// leaking them; on Linux the flag is a no-op (a crashing supervisor orphans agy).
	grp, err := proc.Track(cmd, true)
	if err != nil {
		// Track only fails before Start, which already succeeded above; treat an
		// unexpected failure as fatal, killing agy so it cannot outlive the sentinel.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = jobstore.WriteExitCodeDir(jobDir, jobstore.ExitSpawnFail)
		return err
	}
	defer func() { _ = grp.Close() }()

	// Terminate the agy process group on either an external SIGTERM (cancel from
	// the manager) or the hard timeout, escalating to SIGKILL after a grace
	// window. The hard timeout is the spec's guarantee that a hung agy (which can
	// stall at 0 CPU and ignore its own --print-timeout) can never block forever;
	// effectiveTimeout floors a non-positive meta timeout so the deadline always fires.
	done := make(chan struct{})
	timedOut := make(chan struct{})
	cancelled := make(chan struct{})

	go func() {
		t := time.NewTimer(effectiveTimeout(m.Timeout))
		defer t.Stop()
		select {
		case <-done:
			return
		case <-cancel:
			close(cancelled) // cancel requested by the manager
		case <-t.C:
			close(timedOut)
		}
		_ = grp.Terminate(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(grace):
			// Do not signal a process group that may have already been reaped
			// (and whose pgid could be recycled) once Wait has returned. On Windows
			// the first Terminate already hard-killed the job, so this escalation is
			// a no-op there.
			select {
			case <-done:
			default:
				_ = grp.Terminate(syscall.SIGKILL)
			}
		}
	}()

	// Drain and decode the event stream on its own goroutine. cmd.Wait closes the
	// pipe as soon as it sees the process exit, so the drain must be underway
	// before Wait is called or the stream is truncated and the terminal result
	// event is lost.
	//
	// It cannot be done inline. The read ends only when EVERY holder of the write
	// end closes it, and agy's descendants inherit that descriptor. One that
	// outlives the process group (anything that called setsid, or that the group
	// kill cannot reach) holds the pipe open forever, and an inline drain would
	// then never reach cmd.Wait, never write the exit-code sentinel, and leave the
	// job reported as running for good. Draining concurrently keeps Wait, the hard
	// timeout and the sentinel on a path no descendant can block.
	drained := make(chan streamOutcome, 1)
	go func() { drained <- consumeStream(jobDir, pr, outF) }()

	waitErr := cmd.Wait()
	close(done)

	// agy is reaped. Normally every descendant is gone too, the write end is
	// closed, and the drain has already finished or is microseconds from EOF, so
	// the first branch is taken and nothing is truncated. Only a descendant still
	// holding the inherited descriptor reaches the deadline, and then the read end
	// is closed to release the drain: whatever it decoded is kept, and the
	// exit-code sentinel gets written instead of the job hanging in `running`
	// forever.
	var outcome streamOutcome
	select {
	case outcome = <-drained:
	case <-time.After(drainWait):
		_ = pr.Close()
		outcome = <-drained
	}

	// Record what the stream reported, before the exit-code sentinel: the
	// manager treats the sentinel as the completion signal, so anything written
	// after it could be missed by a poller that observes the sentinel first.
	outcome.persist(jobDir, errF)

	code := 0
	if waitErr != nil {
		if ee, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			code = ee.ExitCode()
			if code < 0 {
				// Killed by a signal. Classify by which signal so an OOM kill or a
				// crash is reported as a failure, not mistaken for a user cancel.
				code = signalExitCode(ee)
			}
		} else {
			code = 1
		}
	}
	// Apply the supervisor's termination overrides: a timeout or a cancel keeps its
	// meaning even when it escalated to SIGKILL (raw 137). timedOut/cancelled are
	// closed before the kill, so they are observable by the time Wait returns.
	// Guarding on waitErr != nil keeps a job that finished naturally at the instant
	// the timer fired (a natural success has waitErr == nil) from being mislabeled.
	return jobstore.WriteExitCodeDir(jobDir, resolveExitCode(code, waitErr != nil, closed(timedOut), closed(cancelled)))
}
