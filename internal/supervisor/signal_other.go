//go:build !linux && !windows

package supervisor

import (
	"os/exec"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// signalExitCode stub so the package builds on platforms without a supervision
// implementation (e.g. macOS). Run refuses early there (proc.Supported is false),
// so this is never reached at runtime.
func signalExitCode(_ *exec.ExitError) int { return jobstore.ExitSIGTERM }
