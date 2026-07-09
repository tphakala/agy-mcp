// Package proc holds the process-tree primitives shared by the manager (which
// spawns the supervisor) and the supervisor (which spawns agy). Linux and macOS
// share the process-group implementation in proc_posix.go; Windows uses Job
// Objects in proc_windows.go; other platforms get the no-op/error stubs in
// proc_other.go so both callers build everywhere and refuse early via Supported.
package proc

import "errors"

// ErrUnsupported is returned by the stubs on platforms without a supervision
// implementation and by callers that refuse before spawning. It is a non-nil
// sentinel on every platform, including Linux, macOS, and Windows where it is
// never returned, so an errors.Is/== comparison against it is always safe: a nil
// error from a successful call must never match it.
var ErrUnsupported = errors.New("agy-mcp: process supervision is only supported on Linux, macOS, and Windows")
