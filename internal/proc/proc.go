// Package proc holds the process-group primitives shared by the manager (which
// spawns and signals the supervisor) and the supervisor (which spawns and
// signals agy). They are meaningful only on Linux; other platforms get the
// no-op/error stubs in proc_other.go so both callers build everywhere and
// refuse early via Supported.
package proc

import "errors"

// ErrUnsupported is returned by the off-Linux stubs and by callers that refuse
// before spawning. It is a non-nil sentinel on every platform, including Linux
// where it is never returned, so an errors.Is/== comparison against it is
// always safe: a nil error from a successful call must never match it.
var ErrUnsupported = errors.New("agy-mcp: process supervision is only supported on Linux")
