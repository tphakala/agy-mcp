//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"testing"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// TestSignalExitCode checks that a signal-terminated agy is classified by which
// signal killed it: SIGTERM/SIGINT are the cancel sentinels, but an abnormal
// death (SIGKILL from an OOM, SIGSEGV from a crash) maps to 128+signal so Status
// reports a failure instead of a clean cancel. The old code mapped every signal
// death to ExitSIGTERM, hiding OOM kills and crashes as cancels.
func TestSignalExitCode(t *testing.T) {
	cases := []struct {
		name string
		sig  syscall.Signal
		want int
	}{
		{"SIGTERM stays a cancel sentinel", syscall.SIGTERM, jobstore.ExitSIGTERM},
		{"SIGINT stays a cancel sentinel", syscall.SIGINT, jobstore.ExitSIGINT},
		{"SIGKILL is a failure, not a cancel", syscall.SIGKILL, 128 + int(syscall.SIGKILL)},
		{"SIGSEGV is a failure, not a cancel", syscall.SIGSEGV, 128 + int(syscall.SIGSEGV)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The process signals itself, so delivery is deterministic (no parent-to-
			// child signal race). The trailing sleep never runs: the kill is fatal.
			cmd := exec.Command("sh", "-c", fmt.Sprintf("kill -%d $$; sleep 60", int(tc.sig)))
			ee, ok := errors.AsType[*exec.ExitError](cmd.Run())
			if !ok {
				t.Fatal("run did not return an *exec.ExitError")
			}
			if got := signalExitCode(ee); got != tc.want {
				t.Errorf("signalExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}
