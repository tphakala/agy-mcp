// Package supervisor runs a single agy process on behalf of agy-mcp and writes
// the exit-code sentinel, so a job survives the death of the manager.
package supervisor

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// killGrace is how long the supervisor waits after SIGTERM before escalating to
// SIGKILL when terminating the agy process group on cancel or timeout.
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

// Run executes the agy process described by jobDir/meta.json. It captures
// stdout to jobDir/out and stderr to jobDir/err, redirects agy stdin from
// /dev/null, and writes jobDir/exit_code on completion (including on cancel).
// Run returns an error only for setup failures, not for a non-zero agy exit.
func Run(jobDir string) error {
	if !platformSupported {
		// Job supervision needs process groups, signal forwarding, and /proc, which
		// only exist on Linux. The non-Linux terminateGroup stub cannot kill agy, so
		// the timeout would "fire" without terminating anything and Run would block in
		// cmd.Wait forever. Refuse here, matching StartJob's guard on the manager side.
		return errPlatformUnsupported
	}
	var m jobstore.Meta
	b, err := os.ReadFile(filepath.Join(jobDir, "meta.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	outF, err := os.Create(filepath.Join(jobDir, "out"))
	if err != nil {
		return err
	}
	defer func() { _ = outF.Close() }()
	errF, err := os.Create(filepath.Join(jobDir, "err"))
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
	setProcessGroup(cmd)

	// Install the SIGTERM handler BEFORE starting agy. A SIGTERM landing in the
	// window between Start and Notify would otherwise kill the supervisor with its
	// default disposition, leaving agy detached with nobody to forward the signal,
	// enforce the timeout, or write the sentinel. The channel is buffered, so a
	// signal that arrives before the forwarding goroutine is reading is held, not
	// lost.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)

	if err := cmd.Start(); err != nil {
		_ = writeExit(jobDir, jobstore.ExitSpawnFail)
		return err
	}

	// Terminate the agy process group on either an external SIGTERM (cancel from
	// the manager) or the hard timeout, escalating to SIGKILL after a grace
	// window. The hard timeout is the spec's guarantee that a hung agy (which can
	// stall at 0 CPU and ignore its own --print-timeout) can never block forever;
	// effectiveTimeout floors a non-positive meta timeout so the deadline always fires.
	done := make(chan struct{})
	timedOut := make(chan struct{})

	go func() {
		t := time.NewTimer(effectiveTimeout(m.Timeout))
		defer t.Stop()
		select {
		case <-done:
			return
		case <-sig:
			// Cancel requested by the manager.
		case <-t.C:
			close(timedOut)
		}
		_ = terminateGroup(cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(killGrace):
			// Do not signal a process group that may have already been reaped
			// (and whose pgid could be recycled) once Wait has returned.
			select {
			case <-done:
			default:
				_ = terminateGroup(cmd.Process.Pid, syscall.SIGKILL)
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
	// If the hard timeout fired, record the timeout sentinel so Status can report
	// a timeout distinctly from a user cancel. Guard on waitErr so a job that
	// finished naturally at the exact instant the timer fired is not mislabeled
	// as a timeout (a natural success has waitErr == nil). timedOut is closed
	// before the kill, so it is observable by the time Wait returns.
	if waitErr != nil {
		select {
		case <-timedOut:
			code = jobstore.ExitTimeout
		default:
		}
	}
	return writeExit(jobDir, code)
}

func writeExit(jobDir string, code int) error {
	return os.WriteFile(filepath.Join(jobDir, "exit_code"), []byte(strconv.Itoa(code)), 0o644)
}
