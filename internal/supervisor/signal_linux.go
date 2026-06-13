//go:build linux

package supervisor

import (
	"os/exec"
	"syscall"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// signalExitCode maps a signal-terminated agy to a sentinel exit code. SIGTERM and
// SIGINT are the cancel sentinels (a manager cancel and the hard-timeout kill both
// forward SIGTERM); any other signal (SIGKILL from an OOM, SIGSEGV from a crash)
// maps to 128+signal so Status reports a failure rather than a clean cancel. A
// wait status that is not a signal death falls back to the cancel sentinel,
// preserving the prior behavior for that unreachable case.
func signalExitCode(ee *exec.ExitError) int {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return jobstore.ExitSIGTERM
	}
	switch ws.Signal() {
	case syscall.SIGTERM:
		return jobstore.ExitSIGTERM
	case syscall.SIGINT:
		return jobstore.ExitSIGINT
	default:
		return 128 + int(ws.Signal())
	}
}
