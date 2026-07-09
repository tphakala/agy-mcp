package supervisor

import "os/exec"

// signalExitCode classifies an agy exit the supervisor did not itself cause. On
// Windows it is effectively unreachable: supervisor.go calls it only when
// exec.ExitError.ExitCode() is negative, and a Windows exit status is an unsigned
// 32-bit value that ExitCode() widens into a non-negative int on the 64-bit target
// (the only way it goes negative is a 0xFFFFFFFF exit on 32-bit Windows). It exists
// for build symmetry with signal_posix.go and returns a generic failure code, never
// a cancel/timeout sentinel: a timeout or cancel is already applied by
// resolveExitCode's overrides before this is consulted, so reaching here means a
// genuine abnormal exit that must be reported as a failure, not a clean cancel.
func signalExitCode(_ *exec.ExitError) int { return 1 }
