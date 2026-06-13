//go:build !linux

package supervisor

import (
	"errors"
	"os/exec"
	"syscall"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// Non-Linux stubs so the package builds on macOS and Windows. Run refuses early
// here (platformSupported is false), so none of these are reached at runtime.

const platformSupported = false

// errPlatformUnsupported is what Run returns off Linux, where job supervision
// (process groups, signal forwarding, /proc liveness) is unavailable.
var errPlatformUnsupported = errors.New("agy-mcp: job supervision is only supported on Linux")

func setProcessGroup(_ *exec.Cmd) {}

func terminateGroup(_ int, _ syscall.Signal) error { return errPlatformUnsupported }

func signalExitCode(_ *exec.ExitError) int { return jobstore.ExitSIGTERM }
