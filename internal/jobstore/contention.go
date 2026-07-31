package jobstore

import (
	"os"
	"time"
)

// Retry budget for a contended job-dir file, spent only when contended reports
// the failure transient (Windows only; see contention_windows.go).
//
// The last attempt is not followed by a sleep, so ONE operation waits at most
// 1+2+4+8+16+32+50+50+50 = 213ms across nine waits before returning the tenth
// attempt's error. That bounds a single call, not a run: the supervisor
// republishes progress.json once per observable step change, so a destination
// that stays contended costs this much per event rather than once.
//
// Both halves of the schedule are load-bearing. The doubling is what stops a
// hopeless operation spinning; the cap is what stops the tail of the budget
// being one long wait.
const (
	contendedAttempts     = 10
	contendedBackoffStart = 1 * time.Millisecond
	contendedBackoffCap   = 50 * time.Millisecond
)

// retryContended runs op, retrying while the platform reports the file it
// touches as held by another process right now rather than genuinely unusable.
//
// Off Windows contended is always false, so op runs exactly once and this is a
// plain call. Windows is the reason it exists, and the reason is not exotic.
//
// Every file in a job dir except out and err is published by writing a temp
// file and renaming it into place (writeFileAtomic). On Windows that replace
// needs DELETE access to the destination, which the OS refuses while any handle
// to it is open without FILE_SHARE_DELETE, and Go's os.Open does not request
// that share mode: os.Open reaches syscall.Open (os/file_windows.go:158), which
// sets FILE_SHARE_READ|FILE_SHARE_WRITE (syscall_windows.go:395). os.Rename is
// MoveFileEx(from, to, MOVEFILE_REPLACE_EXISTING)
// (internal/syscall/windows/syscall_windows.go:366). All three read in go1.26.5.
//
// So a reader and a writer of one job-dir file can each fail because of the
// other, and both directions happen in ordinary use:
//
//   - meta.json. StartJob rewrites it to record the supervisor's pid while the
//     supervisor it just spawned reads it (LoadDir). This is the expensive
//     direction: LoadDir's error aborts the job before it starts.
//   - progress.json. consumeStream republishes it on each observable step
//     change, while Status reads it on every poll of a running job and
//     awaitConversationID reads it every 50ms.
//
// The Go command's own tree hardens both sides of this same hazard for the same
// reason: cmd/internal/robustio wraps Rename ("on Windows retries errors that
// may occur if the file is concurrently read or overwritten") and ReadFile ("on
// Windows retries errors that may occur if the file is concurrently replaced"),
// both citing go.dev/issue/31247 and go.dev/issue/32188. It is the Go command
// rather than the standard library, so it is precedent, not an API to reach for.
//
// The budget bounds one operation, so a caller in a loop multiplies it. The
// largest multiplier is the startup scan, which reads meta.json for every job
// dir and the exit-code sentinel for those whose meta parsed. A contended read
// there is logged and skipped rather than fatal; only the directory scan itself
// fails startup closed. And the usual reason a job dir reads short is a missing
// file, which is never retried, so this costs nothing in the ordinary case.
//
// Considered and not taken: os.Root.Rename reaches a different rename entirely,
// NtSetInformationFile with FILE_RENAME_POSIX_SEMANTICS
// (internal/syscall/windows.Renameat), which might avoid the collision rather
// than retry it. Renameat's FILE_SHARE_DELETE is on the open of the SOURCE, so
// it would be the POSIX-semantics flag rather than the share mode doing the
// work there. It also needs an *os.Root per job dir, which the package-level
// dir-based helpers here do not have, and whether it clears a violation against
// a destination someone else holds open is NOT MEASURED. Retrying is the
// cheaper answer until someone can measure that.
func retryContended(op func() error) error {
	return retryWhile(op, contended, time.Sleep)
}

// renameReplace publishes a temp file over its destination, retrying while the
// destination is contended. See retryContended.
func renameReplace(src, dst string) error {
	return retryContended(func() error { return os.Rename(src, dst) })
}

// readReplaced reads a job-dir file that writeFileAtomic publishes by rename,
// retrying while a concurrent replace makes it briefly unopenable.
//
// Absence is never retried, which is where the predicate deliberately diverges
// from robustio's (that one also treats ERROR_FILE_NOT_FOUND as ephemeral). A
// missing file is a load-bearing answer for three of the four callers: no
// result payload, no exit-code sentinel, no progress yet. Store.ExitCode is the
// one that would hurt, since it is polled throughout a running job precisely
// when the sentinel is absent by definition, so retrying absence would spend
// the whole budget on the hot path of every poll.
func readReplaced(path string) ([]byte, error) {
	var b []byte
	err := retryContended(func() error {
		var rerr error
		b, rerr = os.ReadFile(path)
		return rerr
	})
	return b, err
}

// retryWhile is retryContended's loop with its three environment dependencies
// passed in. They are parameters rather than package-level vars a test could
// swap because a var would be mutable process-wide state existing only for
// tests; passing them keeps every branch, including the Windows-only ones,
// drivable on every platform and drivable without waiting.
//
// The last error is returned unwrapped so a caller's errors.Is still reaches
// the *os.LinkError or *fs.PathError underneath. ReadResultDir relies on that:
// it separates fs.ErrNotExist from a real read failure, and the two mean
// opposite things there.
func retryWhile(op func() error, retryable func(error) bool, sleep func(time.Duration)) error {
	backoff := contendedBackoffStart
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || attempt >= contendedAttempts || !retryable(err) {
			return err
		}
		sleep(backoff)
		backoff *= 2
		if backoff > contendedBackoffCap {
			backoff = contendedBackoffCap
		}
	}
}
