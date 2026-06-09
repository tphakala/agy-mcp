package manager

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// bootID reads the kernel boot id once and caches it. The value is stable for
// the lifetime of the process, so liveness checks (which run per poll) do not
// re-read /proc each time.
var bootID = sync.OnceValue(func() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
})

func readBootID() string { return bootID() }

// processAlive reports whether the job's recorded supervisor PID is still a
// live supervisor process from the current boot.
func (m *Manager) processAlive(meta jobstore.Meta) bool {
	if meta.PID <= 0 {
		return false
	}
	// A PID from a previous boot is meaningless (PID recycling).
	if meta.BootID != "" && meta.BootID != readBootID() {
		return false
	}
	if err := syscall.Kill(meta.PID, 0); err != nil {
		return false
	}
	// Defense in depth: confirm the process is still our supervisor by name.
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(meta.PID) + "/comm")
	if err != nil {
		return false
	}
	name := strings.TrimSpace(string(comm))
	if name != expectedComm(m.cfg.SupervisorExe) {
		return false
	}
	// Same-boot PID recycling: a recycled PID may pass the comm check if the new
	// process is also an agy-mcp. The start time pins identity to this exact
	// process. Only a successful read that mismatches proves recycling; a recorded
	// zero (older meta, or an unreadable /proc at spawn) or a transient read failure
	// here skips the check, so a live job is never falsely read as dead.
	if meta.StartTimeTicks != 0 {
		if cur, ok := readStartTimeTicks(meta.PID); ok && cur != meta.StartTimeTicks {
			return false
		}
	}
	return true
}

// readStartTimeTicks returns the process start time (field 22 of
// /proc/<pid>/stat, in clock ticks since boot) for pid. The second result is
// false on any read or parse failure, so a transient error is never mistaken for
// a recycled pid: callers only act on a successful read.
func readStartTimeTicks(pid int) (uint64, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	return parseStartTimeTicks(b)
}

// parseStartTimeTicks extracts field 22 (starttime) from a /proc/<pid>/stat line.
// Field 2 (comm) is wrapped in parentheses and may itself contain spaces and
// parentheses, so the fields after it are located from the LAST ')'. After that
// ')' the first token is field 3 (state), making field 22 the token at index 19.
func parseStartTimeTicks(stat []byte) (uint64, bool) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 || i+1 >= len(stat) {
		return 0, false
	}
	// Only field 22 is needed, so iterate (FieldsSeq) and stop at it rather than
	// allocating a full slice of every field on each liveness poll.
	const startTimeIdx = 19 // field 22 - field 3 (the first post-')' field)
	idx := 0
	for f := range bytes.FieldsSeq(stat[i+1:]) {
		if idx == startTimeIdx {
			ticks, err := strconv.ParseUint(string(f), 10, 64)
			if err != nil {
				return 0, false
			}
			return ticks, true
		}
		idx++
	}
	return 0, false
}

// expectedComm returns the process name as the kernel records it in
// /proc/<pid>/comm for the given supervisor executable: the basename truncated
// to the kernel's 15-character comm limit.
func expectedComm(exe string) string {
	base := filepath.Base(exe)
	if len(base) > 15 {
		base = base[:15]
	}
	return base
}
