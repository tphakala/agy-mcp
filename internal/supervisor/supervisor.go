// Package supervisor runs a single agy process on behalf of agy-mcp and writes
// the exit-code sentinel, so a job survives the death of the manager.
package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/proc"
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
		// Job supervision needs process groups, signal forwarding, and /proc, which
		// only exist on Linux. The non-Linux proc.TerminateGroup stub cannot kill agy,
		// so the timeout would "fire" without terminating anything and Run would block
		// in cmd.Wait forever. Refuse here, matching StartJob's guard on the manager side.
		return proc.ErrUnsupported
	}
	m, err := jobstore.LoadDir(jobDir)
	if err != nil {
		return err
	}

	// Install the SIGTERM handler first, before opening the job files and starting
	// agy. A SIGTERM (the manager's cancel) landing during this startup window would
	// otherwise kill the supervisor with its default disposition, leaving agy with
	// nobody to forward the signal to, the timeout unenforced, and no sentinel
	// written. The channel is buffered, so a signal that arrives before the
	// forwarding goroutine reads it is held, not lost. Installing it before the job
	// files are created also makes their existence a sound readiness barrier for
	// tests: once out/err exist, the handler is already in place.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

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
	cmd.Stdout = outF
	cmd.Stderr = errF
	// Put agy in its own process group so we can signal it and its children.
	proc.SetGroup(cmd)

	if err := cmd.Start(); err != nil {
		_ = jobstore.WriteExitCodeDir(jobDir, jobstore.ExitSpawnFail)
		return err
	}

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
		case <-sig:
			close(cancelled) // cancel requested by the manager
		case <-t.C:
			close(timedOut)
		}
		_ = proc.TerminateGroup(cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(grace):
			// Do not signal a process group that may have already been reaped
			// (and whose pgid could be recycled) once Wait has returned.
			select {
			case <-done:
			default:
				_ = proc.TerminateGroup(cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}()

	waitErr := cmd.Wait()
	close(done)

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
