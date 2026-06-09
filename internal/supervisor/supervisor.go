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

// Run executes the agy process described by jobDir/meta.json. It captures
// stdout to jobDir/out and stderr to jobDir/err, redirects agy stdin from
// /dev/null, and writes jobDir/exit_code on completion (including on cancel).
// Run returns an error only for setup failures, not for a non-zero agy exit.
func Run(jobDir string) error {
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = writeExit(jobDir, jobstore.ExitSpawnFail)
		return err
	}

	// Terminate the agy process group on either an external SIGTERM (cancel from
	// the manager) or the hard timeout, escalating to SIGKILL after a grace
	// window. The hard timeout is the spec's guarantee that a hung agy (which can
	// stall at 0 CPU and ignore its own --print-timeout) can never block forever.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)
	done := make(chan struct{})
	timedOut := make(chan struct{})

	go func() {
		var deadline <-chan time.Time
		if m.Timeout > 0 {
			t := time.NewTimer(m.Timeout)
			defer t.Stop()
			deadline = t.C
		}
		select {
		case <-done:
			return
		case <-sig:
			// Cancel requested by the manager.
		case <-deadline:
			close(timedOut)
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(killGrace):
			// Do not signal a process group that may have already been reaped
			// (and whose pgid could be recycled) once Wait has returned.
			select {
			case <-done:
			default:
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
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
				code = jobstore.ExitSIGTERM // terminated by signal
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
