//go:build linux

package supervisor

import (
	"os/exec"
	"syscall"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// platformSupported reports whether job supervision (process groups, signal
// forwarding, /proc) runs on this OS. Only Linux is supported; Run refuses early
// elsewhere with errPlatformUnsupported.
const platformSupported = true

// errPlatformUnsupported is what Run returns on an unsupported platform. It is nil
// on Linux; the symbol exists on every platform so Run's guard compiles.
var errPlatformUnsupported error

// setProcessGroup puts agy in its own process group so the supervisor can signal the
// whole group (agy and its children) together.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

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

// terminateGroup sends sig to the entire process group led by pid. A non-positive pid is
// rejected: syscall.Kill(-pid, ...) with pid <= 0 would target the supervisor's own
// process group. Callers always pass a live cmd.Process.Pid (> 0); this guards against a
// future caller.
func terminateGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.EINVAL
	}
	return syscall.Kill(-pid, sig)
}
