//go:build linux || darwin

package testutil

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

// KillProcessGroup SIGKILLs process group pgid and blocks until no member of it
// can still run, so nothing in the group is writing when the caller moves on
// (issue #196). kill(2) returns before its targets exit: MEASURED on macOS
// 26.7.1, kill(-pgid, 0) right after a successful group SIGKILL still succeeds.
// The signal is re-sent on every poll, which also reaches a member that joined
// the group after an earlier pass.
//
// It returns on ESRCH (no member left) or EPERM (no member this process may
// signal; MEASURED on macOS 26.7.1 for a group whose members are all unreaped
// zombies). Any other error, a non-positive pgid (0 would target the caller's
// own group), or a group still signalable at timeout fails the test.
func KillProcessGroup(tb testing.TB, pgid int, timeout time.Duration) {
	tb.Helper()
	if pgid <= 0 {
		tb.Errorf("KillProcessGroup: refusing non-positive pgid %d", pgid)
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
			return
		}
		if err != nil {
			tb.Errorf("KillProcessGroup: kill(-%d, SIGKILL): %v", pgid, err)
			return
		}
		if time.Now().After(deadline) {
			tb.Errorf("process group %d still has live members %v after SIGKILL", pgid, timeout)
			return
		}
		time.Sleep(pollInterval)
	}
}
