package manager

import (
	"runtime"
	"testing"
)

// skipIfWindows skips a test that encodes a Unix-specific assumption (filesystem
// permission semantics, symlinks, forward-slash path formats, or a shell-script
// fake) when the suite runs on Windows. The ported job-supervision paths have
// their own dedicated _windows_test.go coverage; these skips only cover the
// pre-existing Unix-shaped tests.
func skipIfWindows(t *testing.T, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(reason)
	}
}
