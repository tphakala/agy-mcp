package manager

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"golang.org/x/sys/windows"
)

// startTimeMandatory reports whether StartJob must fail when it cannot record a
// supervisor start time. It is false wherever processAlive has a name-based
// liveness fallback (Linux /proc/comm, Windows image name) or never runs a live
// job (the unsupported stub); only darwin (no fallback) sets it true.
const startTimeMandatory = false

// readBootID has no kernel-boot-id analog on Windows, and none is needed:
// readStartTimeTicks returns an absolute wall-clock creation time that already
// differs for a PID recycled after a reboot, so boot pinning is subsumed. It
// returns a fixed non-empty sentinel (rather than "") so StartJob's "boot id
// unreadable; cross-boot liveness degraded" warning, which is meaningful only for
// the Linux boot-relative start time, stays dormant here.
func readBootID() string { return "windows" }

// readStartTimeTicks returns pid's creation time as a uint64 FILETIME (100ns
// ticks since 1601-01-01 UTC). Being absolute wall-clock, it uniquely pins a
// process across reboots, so a recycled PID never matches the original's ticks.
// ok is false on any failure, so a transient error is never mistaken for a
// recycled PID: callers only act on a successful read.
func readStartTimeTicks(pid int) (uint64, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return creationTicks(h)
}

// creationTicks reads the process creation time from an open handle as a packed
// uint64 FILETIME.
func creationTicks(h windows.Handle) (uint64, bool) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), true
}

// processAlive reports whether the job's recorded supervisor PID is still that
// same live supervisor process. It opens the PID, rejects it when the recorded
// creation time no longer matches (PID recycled to a different process), confirms
// the process has not exited, and, only when no creation time was recorded, falls
// back to matching the image name against the supervisor executable.
func (m *Manager) processAlive(meta jobstore.Meta) bool {
	if meta.PID <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(meta.PID))
	if err != nil {
		return false // no such PID (or inaccessible): not our live supervisor
	}
	defer func() { _ = windows.CloseHandle(h) }()

	// A recorded creation time that no longer matches proves the PID was recycled to
	// a different process. When it matches (or a transient read fails) fall through
	// to the still-running check; a definite mismatch is authoritative.
	if meta.StartTimeTicks != 0 {
		if cur, ok := creationTicks(h); ok && cur != meta.StartTimeTicks {
			return false
		}
	}
	// WaitForSingleObject signals (WAIT_OBJECT_0) once the process has exited;
	// WAIT_TIMEOUT means it is still running.
	event, err := windows.WaitForSingleObject(h, 0)
	if err != nil || event != uint32(windows.WAIT_TIMEOUT) {
		return false
	}
	// Without a recorded creation time (older meta, or a transient stat failure)
	// confirm identity by image name, mirroring the Linux comm fallback.
	if meta.StartTimeTicks == 0 {
		return sameImage(h, m.cfg.SupervisorExe)
	}
	return true
}

// sameImage reports whether the process behind h has the same executable
// basename as exe, the Windows analog of the Linux /proc/<pid>/comm check.
func sameImage(h windows.Handle, exe string) bool {
	name, ok := imageName(h)
	if !ok {
		return false
	}
	return strings.EqualFold(filepath.Base(name), filepath.Base(exe))
}

// imageName returns the full image path of an open process handle. It starts with
// a MAX_PATH buffer and grows to the long-path maximum if the image path exceeds
// it, so a supervisor installed under a >260-character path is not misread as dead
// on the name-based liveness fallback. ok is false on any other query failure, so a
// transient error never reads as a matching name.
func imageName(h windows.Handle) (string, bool) {
	for _, n := range [...]uint32{windows.MAX_PATH, 32768} { // 32768 = long-path max + 1
		buf := make([]uint16, n)
		size := n
		err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size)
		if err == nil {
			return windows.UTF16ToString(buf[:size]), true
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", false
		}
	}
	return "", false
}
