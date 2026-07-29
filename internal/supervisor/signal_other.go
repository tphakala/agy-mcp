//go:build !linux && !windows && !darwin

package supervisor

import (
	"os/exec"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// signalExitCode stub so the package builds on platforms without a supervision
// implementation (e.g. FreeBSD). Run refuses early there (proc.Supported is false),
// so this is never reached at runtime.
func signalExitCode(_ *exec.ExitError) int { return jobstore.ExitSIGTERM }
