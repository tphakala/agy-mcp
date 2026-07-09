//go:build darwin

package manager

import (
	"syscall"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"golang.org/x/sys/unix"
)

// startTimeMandatory is true on darwin: processAlive has no /proc/comm-style
// name fallback (see the StartTimeTicks==0 branch in processAlive), so a
// darwin-started job must carry a start time or a PID-recycled impostor could
// not be told apart from the real supervisor. StartJob enforces this at spawn.
const startTimeMandatory = true

// readBootID returns a fixed sentinel on darwin. macOS has no kernel boot id,
// and none is needed: readStartTimeTicks returns an absolute wall-clock start
// time that already differs for a PID recycled after a reboot, so boot pinning
// is subsumed (mirrors liveness_windows.go). The non-empty value keeps
// StartJob's "boot id unreadable" warning dormant.
func readBootID() string { return "darwin" }

// readStartTimeTicks returns pid's start time as microseconds since the Unix
// epoch, read from kp_proc.p_starttime via the typed kern.proc.pid accessor
// unix.SysctlKinfoProc. Being an absolute wall-clock value it uniquely pins a
// process across reboots, so a recycled PID never matches the original's value.
//
// A just-forked child we still hold is readable immediately, but a transient
// sysctl error (e.g. an EINTR from a signal in the window right after fork) can
// still occur, so retry a few times before concluding the read failed; a
// genuinely dead PID errors on every attempt and correctly yields (0, false).
// ok is false on any failure, so a transient error is never mistaken for a
// recycled PID: callers only act on a successful read.
func readStartTimeTicks(pid int) (uint64, bool) {
	for range 3 {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err == nil {
			st := kp.Proc.P_starttime
			return uint64(st.Sec)*1_000_000 + uint64(st.Usec), true
		}
	}
	return 0, false
}

// processAlive reports whether the job's recorded supervisor PID is still that
// same live process, pinned by its absolute start time.
func (m *Manager) processAlive(meta jobstore.Meta) bool {
	if meta.PID <= 0 {
		return false
	}
	// kill(pid, 0) probes existence without signaling. Any error (ESRCH: no such
	// process; EPERM: a different user's process recycled the PID) means it is not
	// our live supervisor. Mirrors the Linux kill(pid,0) probe.
	if err := syscall.Kill(meta.PID, 0); err != nil {
		return false
	}
	if meta.StartTimeTicks == 0 {
		// Fail closed: darwin has no name fallback, and the spawn guarantee
		// (manager.go, gated by startTimeMandatory) means a job this build started
		// always has a start time. A 0 here is foreign or corrupt meta, correctly
		// treated as collectable.
		return false
	}
	// A definite mismatch proves PID recycling. A transient read failure
	// (ok=false) does NOT force a "dead" verdict: the PID exists, so treat it as
	// alive for this poll, exactly as liveness_windows.go does.
	if cur, ok := readStartTimeTicks(meta.PID); ok {
		return cur == meta.StartTimeTicks
	}
	return true
}
