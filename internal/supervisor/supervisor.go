// Package supervisor runs a single agy process on behalf of agy-mcp and writes
// the exit-code sentinel, so a job survives the death of the manager.
package supervisor

import (
	"errors"
	"os"
	"os/exec"
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
	return run(jobDir, killGrace)
}

// run is Run with an injectable SIGTERM->SIGKILL grace, so a test can exercise
// the escalation without the 10s production wait. Passing grace as a parameter
// (rather than mutating a package global) keeps the timer goroutine's read
// race-free.
func run(jobDir string, grace time.Duration) error {
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

	cmd := exec.Command(m.AgyPath, m.Args...)
	cmd.Dir = m.Cwd
	cmd.Env = os.Environ() // agy needs HOME/PATH and its OAuth/API credentials
	cmd.Stdin = devnull
	cmd.Stderr = errF
	// agy runs with --output-format stream-json, so stdout is an event stream to
	// decode rather than text to copy. Reading it is what makes the conversation
	// id observable while the run is still going (the init event carries it) and
	// what turns an interrupted run into recoverable partial text.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// Put agy in its own process group / job so the whole tree can be terminated
	// together on cancel or timeout.
	proc.ConfigureGroup(cmd)

	if err := cmd.Start(); err != nil {
		_ = jobstore.WriteExitCodeDir(jobDir, jobstore.ExitSpawnFail)
		return err
	}
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

	// Drain and decode the event stream before Wait. cmd.Wait closes the pipe as
	// soon as it sees the process exit, so calling it first would truncate the
	// stream and lose the terminal result event. A hung agy cannot park this
	// read: the timer goroutine above terminates the process group, which closes
	// the pipe and ends the loop.
	outcome := consumeStream(jobDir, stdout, outF)

	waitErr := cmd.Wait()
	close(done)

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
