package jobstore

import (
	"errors"

	"golang.org/x/sys/windows"
)

// contended reports whether a failed job-dir operation is worth retrying: the
// file is held by someone else right now, rather than genuinely unusable.
//
// These are the errnos the Go command's own tree retries for this hazard.
// cmd/internal/robustio/robustio_windows.go's isEphemeralError matches
// ERROR_ACCESS_DENIED, ERROR_FILE_NOT_FOUND and ERROR_SHARING_VIOLATION, citing
// go.dev/issue/31247 and go.dev/issue/32188. ERROR_FILE_NOT_FOUND is
// deliberately left out here; readReplaced says why absence has to stay a fast
// answer.
//
// ERROR_ACCESS_DENIED is the ambiguous one. A destination carrying
// FILE_ATTRIBUTE_READONLY, or a DACL of its own denying DELETE, is denied
// permanently, and nothing here can tell that apart from a momentary hold. It
// is included because cmd/internal/robustio includes it, and because the cost of
// being wrong is bounded by the budget rather than open-ended; not because the
// denial is known to be transient.
//
// NOT MEASURED: which of the two a held destination actually produces on a real
// Windows host. Covering both is what keeps the predicate from depending on
// that answer.
//
// os.Rename wraps the errno in *os.LinkError and os.ReadFile in *fs.PathError;
// errors.Is unwraps either. Both constants are declared syscall.Errno in
// x/sys/windows, so the comparison is against the same type os returns, and
// os/os_windows_test.go matches ERROR_SHARING_VIOLATION this same way.
func contended(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
