package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

// TestStatusInterruptedNoOutput: a job whose process is gone with no sentinel
// and no out file is a genuine interruption, reported failed (not done). This
// is the branch TestStatusInterruptedAfterReboot does not cover (that one has
// recovered output and asserts done).
func TestStatusInterruptedNoOutput(t *testing.T) {
	m := newManager(t, managerOpts{})
	// Dead PID from a previous boot, and no out/err/exit_code files at all.
	if _, err := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), PID: 999999, BootID: "old-boot"}); err != nil {
		t.Fatal(err)
	}

	st, _ := m.Status("j")
	if st.State != StateFailed {
		t.Fatalf("state = %q, want failed (interrupted, no output)", st.State)
	}
	if !strings.Contains(st.Error, "interrupted") {
		t.Fatalf("error = %q, want it to mention the interruption", st.Error)
	}
	if st.Partial {
		t.Fatalf("a no-output interruption is not a partial result: %+v", st)
	}
}

// TestErrorSummaryTruncatesOnUTF8Boundary: when the trailing stderr is larger
// than errTailBytes and the cut falls mid-rune, the reported error is advanced
// to a valid UTF-8 boundary rather than emitting a split multi-byte rune.
func TestErrorSummaryTruncatesOnUTF8Boundary(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	// 3-byte runes so the last errTailBytes window starts mid-rune (2000 % 3 != 0).
	content := strings.Repeat("€", 1000) // 3000 bytes
	if err := os.WriteFile(filepath.Join(dir, "err"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = m.store.WriteExitCode("j", 5)

	st, _ := m.Status("j")
	if st.State != StateFailed {
		t.Fatalf("state = %q, want failed", st.State)
	}
	if !utf8.ValidString(st.Error) {
		t.Fatalf("error is not valid UTF-8 (tail split mid-rune): %q", st.Error)
	}
	if len(st.Error) > len("exit 5: ")+errTailBytes {
		t.Fatalf("error length %d exceeds the tail bound", len(st.Error))
	}
}

func TestStatusDone(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	writeResultPayload(t, dir, streamjson.Result{
		ConversationID: "cid-1",
		Status:         streamjson.StatusSuccess,
		Response:       "the review",
		NumTurns:       2,
		Usage:          &streamjson.Usage{TotalTokens: 42},
	})
	_ = m.store.WriteExitCode("j", 0)

	st, err := m.Status("j")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateDone || st.Result != "the review" {
		t.Fatalf("status = %+v", st)
	}
	if st.Partial {
		t.Fatalf("a job with a terminal result must not be marked partial: %+v", st)
	}
	if st.ConversationID != "cid-1" {
		t.Fatalf("conversation_id = %q, want cid-1 from the result payload", st.ConversationID)
	}
	if st.NumTurns != 2 || st.Usage == nil || st.Usage.TotalTokens != 42 {
		t.Fatalf("accounting not surfaced: %+v", st)
	}
}

// writeResultPayload stages the terminal result the supervisor would have
// written for a job.
func writeResultPayload(t *testing.T, dir string, res streamjson.Result) {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobstore.WriteResultDir(dir, b); err != nil {
		t.Fatal(err)
	}
}

// A clean exit with no terminal result event reports whatever text streamed,
// flagged partial: agy finished, but the answer it summarized never arrived.
func TestStatusDoneWithoutResultIsPartial(t *testing.T) {
	m := newManager(t, managerOpts{})
	// Args carrying the output format mark this as a job this build started, so a
	// missing payload really does mean the run was cut short.
	dir, _ := m.store.Create(jobstore.Meta{
		ID: "j", StartedAt: time.Now(), BootID: readBootID(),
		Args: []string{outputFormatFlag, streamJSONFormat, "-p", "hi"},
	})
	_ = os.WriteFile(filepath.Join(dir, "out"), []byte("half an answer"), 0o644)
	_ = m.store.WriteExitCode("j", 0)

	st, err := m.Status("j")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateDone || st.Result != "half an answer" {
		t.Fatalf("status = %+v", st)
	}
	if !st.Partial {
		t.Fatalf("a job with no terminal result must be marked partial: %+v", st)
	}
}

// A job left behind by an older agy-mcp has a complete plain-text out and no
// result payload, because that build never wrote one. Reporting it partial would
// tell the caller a finished answer may be truncated, for the whole TTL after an
// upgrade.
func TestStatusLegacyJobIsNotPartial(t *testing.T) {
	m := newManager(t, managerOpts{})
	// No output-format flag: the shape an older binary persisted.
	dir, _ := m.store.Create(jobstore.Meta{
		ID: "j", StartedAt: time.Now(), BootID: readBootID(),
		Args: []string{"--dangerously-skip-permissions", "-p", "hi"},
	})
	_ = os.WriteFile(filepath.Join(dir, "out"), []byte("a complete v1 answer"), 0o644)
	_ = m.store.WriteExitCode("j", 0)

	st, err := m.Status("j")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateDone || st.Result != "a complete v1 answer" {
		t.Fatalf("status = %+v", st)
	}
	if st.Partial {
		t.Fatalf("a job from an older build must not be reported partial: %+v", st)
	}
}

// A cancelled run never reaches a terminal result, so the text that did stream
// is the only answer there will be. Reporting nothing would discard it.
func TestStatusCancelledCarriesStreamedText(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "out"), []byte("got this far"), 0o644)
	_ = m.store.WriteExitCode("j", jobstore.ExitSIGTERM)

	st, err := m.Status("j")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateCancelled {
		t.Fatalf("state = %q, want cancelled", st.State)
	}
	if st.Result != "got this far" || !st.Partial {
		t.Fatalf("cancelled run must carry its streamed text as partial: %+v", st)
	}
}

// A cancelled job carries no result, but it does carry the conversation, so the
// thread it started can still be continued.
func TestStatusCancelledKeepsConversationID(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	if err := jobstore.WriteProgressDir(dir, jobstore.Progress{ConversationID: "cid-mid-run"}); err != nil {
		t.Fatal(err)
	}
	_ = m.store.WriteExitCode("j", jobstore.ExitSIGTERM)

	st, err := m.Status("j")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateCancelled {
		t.Fatalf("state = %q, want cancelled", st.State)
	}
	if st.ConversationID != "cid-mid-run" {
		t.Fatalf("conversation_id = %q, want the id recorded mid-run", st.ConversationID)
	}
}

func TestStatusFailed(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "err"), []byte("boom"), 0o644)
	_ = m.store.WriteExitCode("j", 5)

	st, _ := m.Status("j")
	if st.State != StateFailed || st.Error == "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestStatusTimedOut(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "err"), []byte("partial"), 0o644)
	_ = m.store.WriteExitCode("j", jobstore.ExitTimeout)

	st, _ := m.Status("j")
	if st.State != StateFailed || !strings.Contains(st.Error, "timeout") {
		t.Fatalf("status = %+v, want failed with a timeout error", st)
	}
}

func TestTailFileReturnsRealEnd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "err")
	// Content longer than the requested tail; the tail must come from the END,
	// not the start (the bug: an io.LimitReader from offset 0 keeps the first N).
	content := strings.Repeat("A", 5000) + "THE-REAL-END"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(p, 20)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if got != content[len(content)-20:] {
		t.Fatalf("tail = %q, want the last 20 bytes", got)
	}
	if !strings.HasSuffix(got, "THE-REAL-END") {
		t.Fatalf("tail %q is not from the real end of the file", got)
	}
}

func TestTailFileShorterThanRequested(t *testing.T) {
	p := filepath.Join(t.TempDir(), "err")
	if err := os.WriteFile(p, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(p, 4096)
	if err != nil {
		t.Fatalf("tailFile: %v", err)
	}
	if got != "short" {
		t.Fatalf("tail = %q, want the whole short file", got)
	}
}

// TestStatusDoneButOutputUnreadable: a job that exited 0 whose out file cannot
// be read must report failed, not done with an empty result. Making out a
// directory lets os.Open succeed while the read fails, exposing the old
// readFile that collapsed every IO error into "".
func TestStatusDoneButOutputUnreadable(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = m.store.WriteExitCode("j", 0)

	st, _ := m.Status("j")
	if st.State != StateFailed || st.Error == "" {
		t.Fatalf("status = %+v, want failed when the output file cannot be read", st)
	}
}

// TestStatusSpawnFail: ExitSpawnFail (127) with no stderr (a true spawn failure)
// gets a dedicated message instead of a bare "exit 127:".
func TestStatusSpawnFail(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "err"), []byte(""), 0o644)
	_ = m.store.WriteExitCode("j", jobstore.ExitSpawnFail)

	st, _ := m.Status("j")
	if st.State != StateFailed {
		t.Fatalf("state = %q, want failed", st.State)
	}
	if !strings.Contains(st.Error, "could not exec the agy binary") {
		t.Fatalf("error = %q, want a dedicated spawn-failure message", st.Error)
	}
}

// TestStatusExit127SurfacesStderr: 127 is also a valid agy exit code, so when
// agy itself exits 127 (with stderr) the message must surface that stderr rather
// than masking it behind the spawn-failure text.
func TestStatusExit127SurfacesStderr(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "err"), []byte("agy: internal tool not found\n"), 0o644)
	_ = m.store.WriteExitCode("j", jobstore.ExitSpawnFail)

	st, _ := m.Status("j")
	if st.State != StateFailed || !strings.Contains(st.Error, "internal tool not found") {
		t.Fatalf("error = %q, want it to surface agy's stderr for a real 127 exit", st.Error)
	}
}

func TestStatusInterruptedAfterReboot(t *testing.T) {
	m := newManager(t, managerOpts{})
	// BootID differs from current -> the recorded PID is from a previous boot.
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), PID: 999999, BootID: "old-boot"})
	_ = os.WriteFile(filepath.Join(dir, "out"), []byte("partial"), 0o644)

	st, _ := m.Status("j")
	if st.State != StateDone { // no sentinel, but output present and process cannot be alive
		t.Fatalf("state = %q, want done (recovered output)", st.State)
	}
	if st.Result != "partial" {
		t.Fatalf("result = %q", st.Result)
	}
	// The supervisor never wrote a sentinel, so the recovered output may be
	// truncated; it must be flagged so a caller does not treat it as complete.
	if !st.Partial {
		t.Fatalf("recovered output without a sentinel must be marked partial: %+v", st)
	}
}

// TestStatusElapsedFrozenAtCompletion: a terminal job's elapsed must reflect the
// run's real duration (start to the sentinel's completion time), not an
// ever-growing time.Since(StartedAt) for a job that finished long ago.
func TestStatusElapsedFrozenAtCompletion(t *testing.T) {
	m := newManager(t, managerOpts{})
	start := time.Now().Add(-time.Hour)
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: start, BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "out"), []byte("done"), 0o644)
	_ = m.store.WriteExitCode("j", 0)
	// Pin the sentinel mtime to 10 minutes after start: completion is well in the
	// past, so a correct Elapsed is ~10m, not the ~1h time.Since(start) would give.
	end := start.Add(10 * time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "exit_code"), end, end); err != nil {
		t.Fatal(err)
	}

	st, _ := m.Status("j")
	if st.State != StateDone {
		t.Fatalf("state = %q, want done", st.State)
	}
	if d := st.Elapsed; d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("elapsed = %v, want ~10m frozen at completion (not time.Since start)", d)
	}
}

// TestStatusRecoveredElapsedFrozen: a job recovered without a completion
// sentinel (process gone, output present) is terminal, so its elapsed must
// freeze at the best available end time (the out file's mtime) rather than
// growing forever as time.Since(StartedAt).
func TestStatusRecoveredElapsedFrozen(t *testing.T) {
	m := newManager(t, managerOpts{})
	start := time.Now().Add(-time.Hour)
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: start, PID: 999999, BootID: "old-boot"})
	outPath := filepath.Join(dir, "out")
	_ = os.WriteFile(outPath, []byte("recovered"), 0o644)
	// Pin the out mtime to 10 minutes after start; a correct elapsed is ~10m, not
	// the ~1h time.Since(start) would give for a job that "finished" an hour ago.
	end := start.Add(10 * time.Minute)
	if err := os.Chtimes(outPath, end, end); err != nil {
		t.Fatal(err)
	}

	st, _ := m.Status("j")
	if st.State != StateDone || !st.Partial {
		t.Fatalf("status = %+v, want done+partial", st)
	}
	if d := st.Elapsed; d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("elapsed = %v, want ~10m frozen at the recovered end", d)
	}
}

// TestStatusElapsedClampedOnClockSkew: when the recorded completion time
// implausibly precedes StartedAt (clock skew), a terminal job's elapsed must
// stay frozen (clamped to 0), not fall back to an ever-growing time.Since.
func TestStatusElapsedClampedOnClockSkew(t *testing.T) {
	m := newManager(t, managerOpts{})
	start := time.Now().Add(-time.Hour)
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: start, BootID: readBootID()})
	_ = os.WriteFile(filepath.Join(dir, "out"), []byte("done"), 0o644)
	_ = m.store.WriteExitCode("j", 0)
	skewed := start.Add(-time.Hour) // sentinel mtime before StartedAt
	if err := os.Chtimes(filepath.Join(dir, "exit_code"), skewed, skewed); err != nil {
		t.Fatal(err)
	}

	st, _ := m.Status("j")
	if st.State != StateDone {
		t.Fatalf("state = %q, want done", st.State)
	}
	if st.Elapsed != 0 {
		t.Fatalf("elapsed = %v, want 0 (clamped under clock skew, not time.Since)", st.Elapsed)
	}
}

// TestStateMatchesStatusState: the cheap State accessor (used by agy_cancel,
// which only needs the state and must not pay to read a large out file) must
// agree with Status's full state across every terminal exit code.
func TestStateMatchesStatusState(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{0, StateDone},
		{jobstore.ExitSIGTERM, StateCancelled},
		{jobstore.ExitSIGINT, StateCancelled},
		{jobstore.ExitTimeout, StateFailed},
		{jobstore.ExitSpawnFail, StateFailed},
		{5, StateFailed},
	}
	m := newManager(t, managerOpts{})
	for _, c := range cases {
		id := "code-" + strconv.Itoa(c.code)
		dir, _ := m.store.Create(jobstore.Meta{ID: id, StartedAt: time.Now(), BootID: readBootID()})
		_ = os.WriteFile(filepath.Join(dir, "out"), []byte("x"), 0o644)
		_ = m.store.WriteExitCode(id, c.code)

		gotState, err := m.State(id)
		if err != nil {
			t.Fatalf("State(%d): %v", c.code, err)
		}
		if gotState != c.want {
			t.Fatalf("State for exit %d = %q, want %q", c.code, gotState, c.want)
		}
		st, _ := m.Status(id)
		if gotState != st.State {
			t.Fatalf("State %q disagrees with Status.State %q for exit %d", gotState, st.State, c.code)
		}
	}
}

// TestStateMatchesStatusOnUnreadableCleanExit pins the one edge case where a
// naive cheap path would diverge: a job that exited 0 but whose out file cannot
// be read. Status downgrades that to failed (an unreadable success is not a
// success), and State must report the same, not a bare "done" from the code.
// Making out a directory lets os.Open succeed while the read fails.
func TestStateMatchesStatusOnUnreadableCleanExit(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = m.store.WriteExitCode("j", 0)

	gotState, err := m.State("j")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := m.Status("j")
	if gotState != StateFailed {
		t.Fatalf("State = %q, want failed when a clean exit's output is unreadable", gotState)
	}
	if gotState != st.State {
		t.Fatalf("State %q disagrees with Status.State %q", gotState, st.State)
	}
}

// A result payload whose recognized fields are all absent is indeterminate, not
// a completed empty answer. Reading absence alone as success would hand the
// caller "done" with nothing in it the moment agy renames or restructures those
// fields, which is precisely what the version floor cannot prevent.
func TestStatusEmptyResultPayloadIsNotSuccess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		res     streamjson.Result
		want    string
		wantRes string
	}{
		{"no status and no response", streamjson.Result{}, StateFailed, ""},
		{"no status but a response is recoverable", streamjson.Result{Response: "an answer"}, StateDone, "an answer"},
		{"explicit success", streamjson.Result{Status: streamjson.StatusSuccess, Response: "ok"}, StateDone, "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, managerOpts{})
			dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), BootID: readBootID()})
			writeResultPayload(t, dir, tc.res)
			_ = m.store.WriteExitCode("j", 0)

			st, err := m.Status("j")
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.want || st.Result != tc.wantRes {
				t.Fatalf("state = %q result = %q, want %q / %q", st.State, st.Result, tc.want, tc.wantRes)
			}
			if tc.want == StateFailed && st.Error == "" {
				t.Fatal("an indeterminate payload must carry an explanation")
			}
		})
	}
}
