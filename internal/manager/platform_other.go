//go:build !linux

package manager

import (
	"errors"
	"os/exec"
)

// platformSupported is false off Linux: job supervision relies on process groups and
// /proc. StartJob checks this and returns errPlatformUnsupported before spawning, so the
// no-op/error stubs below are never exercised in a real run; they exist so the manager
// package builds on macOS and Windows (the MCP server, list_models, and list_sessions
// still work there).
const platformSupported = false

var errPlatformUnsupported = errors.New("agy-mcp job supervision is only supported on Linux")

func setProcessGroup(_ *exec.Cmd) {}

func terminateGroup(_ int) error { return errPlatformUnsupported }

func signalProcess(_ int) error { return errPlatformUnsupported }
