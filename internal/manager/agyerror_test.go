package manager

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

const (
	// measuredAgyErrorLine1 is line 1 of the stderr agy 1.3.1 wrote for a model
	// that is not available in the account's region, without its "error: " prefix
	// (which is what agy puts in the payload's error field).
	measuredAgyErrorLine1 = "Selected model is not supported in the selected location. For a full list of available locations and deployment endpoints, please refer to Antigravity documentation at https://antigravity.google/docs/enterprise."
	// measuredAgyErrorID is a redacted stand-in for the measured error_id shape.
	measuredAgyErrorID = "00000000-0000-4000-8000-000000000000-1"
	// wireAgyErrorPrefix is the literal text agy writes before the object. It is
	// spelled out here rather than taken from agyErrorPrefix, so a typo in the
	// production constant fails the tests instead of moving the fixtures with it.
	wireAgyErrorPrefix = "AGY_ERROR: "
)

// measuredAgyErrorJSON is the object on line 2, with the project and region
// redacted. short_error holds backticks, so it is an interpreted string.
var measuredAgyErrorJSON = `{"short_error":"NOT_FOUND (code 404): Publisher model ` + "`projects/example-project/locations/example-region/publishers/google/models/gemini-3.1-pro-preview`" +
	` was not found or your project does not have access to it. Ensure you are using a valid model name and that the model is available in the specified region.","retryable":false,"error_id":"` + measuredAgyErrorID + `"}`

// measuredAgyErrorStderr is the two-line stderr agy 1.3.1 wrote (exit 3).
var measuredAgyErrorStderr = "error: " + measuredAgyErrorLine1 + "\n" + wireAgyErrorPrefix + measuredAgyErrorJSON + "\n"

func TestParseAgyErrorTail(t *testing.T) {
	line := func(obj string) string { return wireAgyErrorPrefix + obj + "\n" }
	for _, tc := range []struct {
		name          string
		tail          string
		truncated     bool
		wantOK        bool
		wantRetryable *bool
		wantErrorID   string
		wantStatus    string
	}{
		{name: "measured 1.3.1 stderr", tail: measuredAgyErrorStderr, wantOK: true, wantRetryable: new(false), wantErrorID: measuredAgyErrorID, wantStatus: "NOT_FOUND"},
		{name: "1.2.9 variant with a capitalised first line", tail: "Error: " + measuredAgyErrorLine1 + "\n" + wireAgyErrorPrefix + measuredAgyErrorJSON + "\n", wantOK: true, wantRetryable: new(false), wantErrorID: measuredAgyErrorID, wantStatus: "NOT_FOUND"},
		{name: "no AGY_ERROR line", tail: "error: " + measuredAgyErrorLine1 + "\n"},
		{name: "prefix indented", tail: "  " + line(measuredAgyErrorJSON)},
		{name: "prefix not at column 0", tail: "x " + line(measuredAgyErrorJSON)},
		{name: "lowercase prefix", tail: "agy_error: " + measuredAgyErrorJSON + "\n"},
		{name: "JSON cut mid-string", tail: wireAgyErrorPrefix + measuredAgyErrorJSON[:60]},
		{name: "trailing garbage after the object", tail: line(measuredAgyErrorJSON + " junk")},
		{name: "null payload", tail: line("null")},
		{name: "array payload", tail: line(`[{"retryable":false}]`)},
		{name: "string payload", tail: line(`"x"`)},
		{name: "retryable as a string", tail: line(`{"retryable":"false","error_id":"e-1"}`)},
		{name: "empty object", tail: line(`{}`)},
		{name: "retryable missing, error_id present", tail: line(`{"error_id":"e-1"}`), wantOK: true, wantErrorID: "e-1"},
		{name: "status prefix without a code", tail: line(`{"short_error":"NOT_FOUND: x","retryable":true}`), wantOK: true, wantRetryable: new(true)},
		{name: "unrecognized short_error", tail: line(`{"short_error":"oops","retryable":true}`), wantOK: true, wantRetryable: new(true)},
		{name: "synthetic RESOURCE_EXHAUSTED, no captured sample", tail: line(`{"short_error":"RESOURCE_EXHAUSTED (code 429): try later","retryable":true}`), wantOK: true, wantRetryable: new(true), wantStatus: "RESOURCE_EXHAUSTED"},
		{name: "two lines, last valid wins", tail: line(`{"retryable":true,"error_id":"first"}`) + line(`{"retryable":false,"error_id":"last"}`), wantOK: true, wantRetryable: new(false), wantErrorID: "last"},
		{name: "two lines, last malformed, no fallback", tail: line(`{"retryable":true,"error_id":"first"}`) + wireAgyErrorPrefix + `{"retryable":fal`},
		{name: "CRLF line endings", tail: strings.ReplaceAll(measuredAgyErrorStderr, "\n", "\r\n"), wantOK: true, wantRetryable: new(false), wantErrorID: measuredAgyErrorID, wantStatus: "NOT_FOUND"},
		{name: "two spaces after the prefix", tail: wireAgyErrorPrefix + " " + measuredAgyErrorJSON + "\n", wantOK: true, wantRetryable: new(false), wantErrorID: measuredAgyErrorID, wantStatus: "NOT_FOUND"},
		// encoding/json matches keys case-insensitively; accepted and pinned so a
		// move to strict decoding is a visible decision.
		{name: "upper-case key is accepted", tail: line(`{"RETRYABLE":false}`), wantOK: true, wantRetryable: new(false)},
		{name: "error_id with a space is dropped", tail: line(`{"retryable":false,"error_id":"a b"}`), wantOK: true, wantRetryable: new(false)},
		{name: "error_id with a slash is dropped", tail: line(`{"retryable":false,"error_id":"a/b"}`), wantOK: true, wantRetryable: new(false)},
		{name: "error_id of 129 bytes is dropped", tail: line(`{"retryable":false,"error_id":"` + strings.Repeat("a", 129) + `"}`), wantOK: true, wantRetryable: new(false)},
		{name: "truncated: a complete-looking first line is dropped", tail: line(`{"retryable":false}`) + "more\n", truncated: true},
		{name: "truncated: a later line is kept", tail: "fragment\n" + line(`{"retryable":false}`), truncated: true, wantOK: true, wantRetryable: new(false)},
		{name: "truncated with no newline", tail: "fragment", truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, ok := parseAgyErrorTail(tc.tail, tc.truncated)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (info %+v)", ok, tc.wantOK, info)
			}
			if !ok {
				if info != (agyErrorInfo{}) {
					t.Fatalf("a failed parse must return the zero info, got %+v", info)
				}
				return
			}
			if !equalBoolPtr(info.retryable, tc.wantRetryable) {
				t.Fatalf("retryable = %v, want %v", derefBool(info.retryable), derefBool(tc.wantRetryable))
			}
			if info.errorID != tc.wantErrorID {
				t.Errorf("errorID = %q, want %q", info.errorID, tc.wantErrorID)
			}
			if info.status != tc.wantStatus {
				t.Errorf("status = %q, want %q", info.status, tc.wantStatus)
			}
		})
	}
}

func TestReadAgyErrorFromJobDir(t *testing.T) {
	filler := strings.Repeat("some agy chatter line\n", 150) // 3300 bytes
	for _, tc := range []struct {
		name   string
		stderr string // "" writes no err file
		wantOK bool
	}{
		{name: "no err file"},
		{name: "line at the end of a large file", stderr: filler + measuredAgyErrorStderr, wantOK: true},
		{name: "a single line longer than the tail", stderr: wireAgyErrorPrefix + `{"retryable":false,"error_id":"e-1","pad":"` + strings.Repeat("x", errTailBytes) + `"}` + "\n"},
		{name: "exactly errTailBytes with the line first", stderr: exactSizeWithLineFirst(errTailBytes), wantOK: true},
		{name: "errTailBytes+1 cuts the first line", stderr: exactSizeWithLineFirst(errTailBytes + 1)},
		// The window starts on the AGY_ERROR prefix in both rows below; only the byte
		// before it says whether that is a whole line or the tail of a longer one.
		{name: "a newline before the window keeps a whole first line", stderr: "\n" + exactSizeWithLineFirst(errTailBytes), wantOK: true},
		{name: "a line continuing from before the window is dropped", stderr: "x" + exactSizeWithLineFirst(errTailBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeIfSet(t, jobstore.ErrPath(dir), tc.stderr)
			if _, ok := readAgyError(dir); ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// exactSizeWithLineFirst returns a stderr of exactly size bytes whose first line
// is the measured AGY_ERROR line, padded with filler lines.
func exactSizeWithLineFirst(size int) string {
	first := wireAgyErrorPrefix + measuredAgyErrorJSON + "\n"
	pad := size - len(first)
	// One filler line of pad bytes, newline-terminated.
	return first + strings.Repeat("f", pad-1) + "\n"
}

func TestReadAgyErrorUnreadableStderr(t *testing.T) {
	skipIfWindows(t, "a directory does not make tailFile's read fail on Windows")
	dir := t.TempDir()
	// A directory where the err file belongs makes the read fail with an error
	// other than not-exist.
	if err := os.Mkdir(jobstore.ErrPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := readAgyError(dir); ok {
		t.Fatal("an unreadable stderr must be fail-open (ok == false)")
	}
}

func equalBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func derefBool(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestStatusRecoveredReadsAgyError pins that a job whose supervisor died after
// persisting an ERROR payload gets the same AGY_ERROR treatment as the same job
// with a clean exit sentinel (issue #183).
func TestStatusRecoveredReadsAgyError(t *testing.T) {
	stage := func(t *testing.T, recovered bool) Status {
		t.Helper()
		m := newManager(t, managerOpts{})
		meta := jobstore.Meta{ID: "j", StartedAt: time.Now()}
		if recovered {
			// A dead PID from a previous boot and no sentinel: the recovery path.
			meta.PID, meta.BootID = 999999, "old-boot"
		} else {
			meta.BootID = readBootID()
		}
		dir, err := m.store.Create(meta)
		if err != nil {
			t.Fatal(err)
		}
		writeResultPayload(t, dir, streamjson.Result{Status: streamjson.StatusError, Error: measuredAgyErrorLine1, ConversationID: "c1"})
		writeIfSet(t, jobstore.ErrPath(dir), measuredAgyErrorStderr)
		if !recovered {
			if err := m.store.WriteExitCode("j", 0); err != nil {
				t.Fatal(err)
			}
		}
		st, err := m.Status("j")
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	for _, recovered := range []bool{true, false} {
		st := stage(t, recovered)
		if st.State != StateFailed || st.FailureReason != ReasonAgyError {
			t.Fatalf("recovered=%v: state %q reason %q, want failed/agy_error", recovered, st.State, st.FailureReason)
		}
		if st.Retryable == nil || *st.Retryable || st.ErrorID != measuredAgyErrorID {
			t.Fatalf("recovered=%v: retryable %v error_id %q, want false and %q", recovered, derefBool(st.Retryable), st.ErrorID, measuredAgyErrorID)
		}
	}
}
