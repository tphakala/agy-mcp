//go:build !windows

package jobstore

import "os"

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

// fsyncDir fsyncs dir itself, so a rename INTO dir (writeFileAtomicDurable) is
// flushed and not just the renamed file's data. The standard POSIX durable-rename
// recipe is fsync(temp), rename, fsync(dir); writeFileAtomic already does the
// first two. This is a plain fsync, the same guarantee tmp.Sync gives the data
// (on darwin full media durability would need F_FULLFSYNC), so the directory
// entry ends up exactly as durable as the file's data, no more. The directory is
// opened read-only and Synced; there is nothing to write to it.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	// Report Sync's error ahead of Close's: a failed directory fsync is the
	// durability failure the caller needs to hear about, and Close would mask it.
	if serr := d.Sync(); serr != nil {
		_ = d.Close()
		return serr
	}
	return d.Close()
}
