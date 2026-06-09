package manager

import (
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
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(meta.PID), "comm"))
	if err != nil {
		return false
	}
	name := strings.TrimSpace(string(comm))
	return name == expectedComm(m.cfg.SupervisorExe)
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
