//go:build !windows

package jobstore

// contended is always false off Windows: rename(2) swaps a directory entry and
// open(2) resolves to an inode, and another process holding the old file open
// blocks neither. A failure here is a real one (a missing directory, a full or
// read-only filesystem, a cross-device path), so retrying would only delay the
// report.
//
// TestContendedIsAlwaysFalse asserts this function's own answer;
// TestWriteFileAtomicReplacesAnOpenFile pins the kernel behaviour that makes it
// the right answer.
//
// The constraint is !windows rather than the unix/_other.go split used
// elsewhere in this repo, and the filename says so rather than saying _other:
// jobstore is platform-agnostic, so a unix tag would leave js/wasm and plan9
// with no contended at all, and an _other.go here would read as the
// unsupported-platform stub that name means in the other packages.
func contended(error) bool { return false }
