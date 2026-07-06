package manager

import (
	"errors"

	"golang.org/x/sys/windows"
)

// flockExclusiveNB takes a non-blocking exclusive lock on the file handle behind
// fd via LockFileEx, the Windows analog of the Linux flock. It returns (true, nil)
// on success, (false, nil) when another handle already holds the lock
// (ERROR_LOCK_VIOLATION, the LOCKFILE_FAIL_IMMEDIATELY result), and (false, err)
// on any other error. The lock covers a fixed one-byte range so every process
// contends on the same bytes, and Windows releases it when the handle is closed,
// matching the Linux "closing the fd drops the lock" model crossLock relies on.
func flockExclusiveNB(fd uintptr) (bool, error) {
	var overlapped windows.Overlapped // offset 0
	err := windows.LockFileEx(
		windows.Handle(fd),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, // reserved
		1, // bytesLow: lock one byte
		0, // bytesHigh
		&overlapped,
	)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	default:
		return false, err
	}
}
