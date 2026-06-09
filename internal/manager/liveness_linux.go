package manager

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// supervisorComm is the process name (/proc/<pid>/comm) of a live job
// supervisor. It is a package variable so a test can repoint liveness at a
// stand-in process if needed.
var supervisorComm = "agy-mcp"

func readBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// processAlive reports whether the job's recorded supervisor PID is still a
// live agy-mcp process from the current boot.
func processAlive(meta jobstore.Meta) bool {
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
	// Defense in depth: confirm the process is still our supervisor.
	comm, err := os.ReadFile(filepath.Join("/proc", itoa(meta.PID), "comm"))
	if err != nil {
		return false
	}
	name := strings.TrimSpace(string(comm))
	return name == supervisorComm
}
