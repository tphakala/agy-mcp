package jobstore

import (
	"errors"
	"testing"
	"time"
)

// errContended stands in for the platform errno meaning "another process holds
// this file right now". A sentinel rather than the real ERROR_SHARING_VIOLATION
// keeps every branch of the loop drivable on every platform: contended is false
// off Windows, so a test feeding it a real errno would exercise the retry on
// exactly one of the three OSes CI runs.
var errContended = errors.New("file contended")

// errDenied is a failure no retry can fix, so the loop must report it at once.
var errDenied = errors.New("read-only filesystem")

// retryCall records one drive of retryWhile: how many times it ran the
// operation, and every duration it was told to sleep for.
type retryCall struct {
	attempts int
	sleeps   []time.Duration
}

// drive runs retryWhile against an operation that returns results in order,
// treating only errContended as retryable and recording sleeps instead of
// taking them. Once results run out the last one repeats, so a caller testing
// the budget need not spell out contendedAttempts entries.
//
// It caps the operation at three times the budget and then hard-fails, so a
// regression removing the attempt ceiling shows up as this assertion rather
// than as a test binary spinning until the package timeout.
func drive(t *testing.T, results ...error) (*retryCall, error) {
	t.Helper()
	if len(results) == 0 {
		t.Fatal("drive needs at least one result; with none the operation has nothing to return")
	}
	rc := &retryCall{}
	err := retryWhile(
		func() error {
			rc.attempts++
			if rc.attempts > 3*contendedAttempts {
				t.Fatalf("operation retried %d times with no ceiling; contendedAttempts is %d", rc.attempts, contendedAttempts)
			}
			if rc.attempts <= len(results) {
				return results[rc.attempts-1]
			}
			return results[len(results)-1]
		},
		func(err error) bool { return errors.Is(err, errContended) },
		func(d time.Duration) { rc.sleeps = append(rc.sleeps, d) },
	)
	return rc, err
}

// An operation that works is one syscall and no waiting: the budget must be
// spent only on failures.
func TestRetryWhileDoesNotSleepWhenTheFirstAttemptSucceeds(t *testing.T) {
	rc, err := drive(t, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rc.attempts != 1 {
		t.Errorf("attempts = %d, want 1", rc.attempts)
	}
	if len(rc.sleeps) != 0 {
		t.Errorf("slept %v on a successful operation, want no waiting", rc.sleeps)
	}
}

// The point of the whole file: a file contended now and free shortly after must
// end up read or replaced, not reported as an error.
func TestRetryWhileSucceedsOnceTheFileIsReleased(t *testing.T) {
	rc, err := drive(t, errContended, errContended, errContended, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil once the file was released", err)
	}
	if rc.attempts != 4 {
		t.Errorf("attempts = %d, want 4", rc.attempts)
	}
	// One wait per retry, never one after the attempt that succeeded.
	if len(rc.sleeps) != 3 {
		t.Errorf("sleeps = %v, want one per retry (3)", rc.sleeps)
	}
}

// A failure the retry cannot fix must cost one syscall and no delay. Without
// the retryable check the caller would wait out the entire budget before being
// told its filesystem is read-only.
func TestRetryWhileReportsANonRetryableErrorAtOnce(t *testing.T) {
	rc, err := drive(t, errDenied)
	if !errors.Is(err, errDenied) {
		t.Fatalf("err = %v, want errDenied", err)
	}
	if rc.attempts != 1 {
		t.Errorf("attempts = %d, want 1: a non-retryable error must not be retried", rc.attempts)
	}
	if len(rc.sleeps) != 0 {
		t.Errorf("slept %v before reporting a non-retryable error", rc.sleeps)
	}
}

// A file that never frees up has to give up, and give up returning the
// underlying error rather than something a caller's errors.Is cannot match.
func TestRetryWhileGivesUpAfterTheBudget(t *testing.T) {
	rc, err := drive(t, errContended)
	if !errors.Is(err, errContended) {
		t.Fatalf("err = %v, want the last operation error (errContended)", err)
	}
	if rc.attempts != contendedAttempts {
		t.Errorf("attempts = %d, want contendedAttempts (%d)", rc.attempts, contendedAttempts)
	}
	if len(rc.sleeps) != contendedAttempts-1 {
		t.Errorf("sleeps = %d, want contendedAttempts-1 (%d): the last attempt must not be followed by a wait",
			len(rc.sleeps), contendedAttempts-1)
	}
}

// The schedule itself, spelled out in literals rather than in terms of the
// constants it is meant to pin. Naming contendedBackoffCap in the wanted values
// would make the last three entries a tautology that survives changing the cap;
// spelling 50ms means changing the cap turns this red.
func TestRetryWhileBackoffDoublesUpToTheCap(t *testing.T) {
	rc, err := drive(t, errContended)
	if !errors.Is(err, errContended) {
		t.Fatalf("err = %v, want errContended", err)
	}
	want := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		4 * time.Millisecond,
		8 * time.Millisecond,
		16 * time.Millisecond,
		32 * time.Millisecond,
		50 * time.Millisecond,
		50 * time.Millisecond,
		50 * time.Millisecond,
	}
	if len(rc.sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", rc.sleeps, want)
	}
	for i, d := range want {
		if rc.sleeps[i] != d {
			t.Errorf("sleep %d = %v, want %v (full schedule %v)", i, rc.sleeps[i], d, rc.sleeps)
		}
	}
	// The exact figure contention.go's doc comment claims. Asserting the total
	// as well as the elements is what keeps that comment honest: a constant
	// change that altered the sum is caught here even if someone also updated
	// the element list to match.
	var total time.Duration
	for _, d := range rc.sleeps {
		total += d
	}
	if wantTotal := 213 * time.Millisecond; total != wantTotal {
		t.Errorf("worst-case wait for one operation = %v, want %v (the figure contention.go documents)", total, wantTotal)
	}
}
