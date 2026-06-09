// Package supervisor runs a single agy process on behalf of agy-mcp and writes
// the exit-code sentinel, so a job survives the death of the manager.
package supervisor

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

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
	defer outF.Close()
	errF, err := os.Create(filepath.Join(jobDir, "err"))
	if err != nil {
		return err
	}
	defer errF.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()

	cmd := exec.Command(m.AgyPath, m.Args...)
	cmd.Dir = m.Cwd
	cmd.Env = os.Environ() // agy needs HOME/PATH and its OAuth/API credentials
	cmd.Stdin = devnull
	cmd.Stdout = outF
	cmd.Stderr = errF
	// Put agy in its own process group so we can signal it and its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = writeExit(jobDir, 127)
		return err
	}

	// Forward SIGTERM (sent by the manager on cancel) to the agy process group.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	defer signal.Stop(sig)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		case <-done:
		}
	}()

	waitErr := cmd.Wait()
	close(done)

	code := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
			if code < 0 {
				code = 143 // terminated by signal
			}
		} else {
			code = 1
		}
	}
	return writeExit(jobDir, code)
}

func writeExit(jobDir string, code int) error {
	return os.WriteFile(filepath.Join(jobDir, "exit_code"), []byte(strconv.Itoa(code)), 0o644)
}
