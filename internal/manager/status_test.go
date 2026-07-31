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

// terminalCase is one staged job directory and the status it must produce.
type terminalCase struct {
	name string
	code int
	// res is the terminal payload; nil means the run never reached one.
	res         *streamjson.Result
	out         string // streamed text on disk, "" writes no out file
	errFile     string // captured stderr, "" writes no err file
	args        []string
	wantState   string
	wantResult  string
	wantPartial bool
	wantErrSub  string
	wantConvID  string // conversation carried off the payload, "" not asserted
	wantTurns   int    // agy's own accounting, 0 not asserted
}

// terminalCases is the whole Result/Partial contract in one table, consumed by
// TestStatusTerminalContract (does the code agree?) and by
// TestStatusTerminalContractTableIsWellFormed (does the table itself?).
//
// It exists because this file regressed under three successive incremental fix
// waves, each of which closed one branch's case and left the identical case
// open in a sibling branch: no single test held all the cases together, so a
// change could satisfy the test it was written for while breaking an untested
// peer. Every terminal path appears here, so that cannot happen silently.
//
// The contract the rows encode:
//
//   - Every terminal state carries whatever answer the run produced. A cancel, a
//     timeout, a crash and a 127 all lose the same work, so none of them may
//     report nothing while a sibling reports text.
//   - Partial is decided by where that text came from, not by the state. A
//     response agy itself marked SUCCESS is complete even if the job was then
//     killed; any other payload status is agy declining to vouch for it; text
//     rebuilt from the stream is partial. The one exception is a job an older
//     build wrote, whose plain-text out really is complete.
//   - Whichever way a run ended, a payload that reached disk still supplies the
//     conversation to continue and agy's own accounting.
func terminalCases() []terminalCase {
	streamArgs := []string{outputFormatFlag, streamJSONFormat, "-p", "hi"}
	legacyArgs := []string{"--dangerously-skip-permissions", "-p", "hi"}
	return []terminalCase{
		// --- clean exit, terminal payload present -------------------------------
		{
			name: "success payload is the complete answer",
			code: 0, res: &streamjson.Result{Status: streamjson.StatusSuccess, Response: "final"},
			out: "streamed", wantState: StateDone, wantResult: "final",
		}, {
			// agy vouched for the run but the payload carried no text, so the stream
			// is the only answer there is; reporting done-and-empty would assert a
			// completed empty answer.
			name: "success payload with no text falls back to the stream",
			code: 0, res: &streamjson.Result{Status: streamjson.StatusSuccess},
			out: "streamed", wantState: StateDone, wantResult: "streamed", wantPartial: true,
		}, {
			// The bug: an ERROR payload's response used to be discarded entirely on a
			// clean exit, while the unrecognized-status branch beside it kept exactly
			// this text.
			name: "error payload keeps the text it carried",
			code: 0, res: &streamjson.Result{Status: streamjson.StatusError, Response: "got this far", Error: "model unavailable"},
			out: "streamed", wantState: StateFailed, wantResult: "got this far", wantPartial: true,
			wantErrSub: "model unavailable",
		}, {
			name: "error payload with no text falls back to the stream",
			code: 0, res: &streamjson.Result{Status: streamjson.StatusError, Error: "model unavailable"},
			out: "streamed", wantState: StateFailed, wantResult: "streamed", wantPartial: true,
			wantErrSub: "model unavailable",
		}, {
			name: "error payload with no message still explains itself",
			code: 0, res: &streamjson.Result{Status: streamjson.StatusError},
			wantState: StateFailed, wantErrSub: "without a message",
		}, {
			// The bug: a response with no status used to report done and NOT partial,
			// so a future agy that renames the status field would have a run cut
			// short by MAX_TURNS reported as a clean, complete answer.
			name: "a response with no status is recoverable but unverified",
			code: 0, res: &streamjson.Result{Response: "an answer"},
			wantState: StateDone, wantResult: "an answer", wantPartial: true,
		}, {
			name: "a payload with no recognized field at all is indeterminate",
			code: 0, res: &streamjson.Result{},
			wantState: StateFailed, wantErrSub: "no status and no response",
		}, {
			name: "an indeterminate payload still falls back to the stream",
			code: 0, res: &streamjson.Result{},
			out: "streamed", wantState: StateFailed, wantResult: "streamed", wantPartial: true,
			wantErrSub: "no status and no response",
		}, {
			name: "an unrecognized status keeps its response",
			code: 0, res: &streamjson.Result{Status: "MAX_TURNS", Response: "as far as I got"},
			wantState: StateFailed, wantResult: "as far as I got", wantPartial: true,
			wantErrSub: "unrecognized result status",
		}, {
			// The no-text sibling of the row above. Every other payload status has
			// one, and the asymmetry is what let this case lose its coverage when
			// four older tests were folded into this table.
			name: "an unrecognized status with no response",
			code: 0, res: &streamjson.Result{Status: "MAX_TURNS"},
			wantState: StateFailed, wantErrSub: "unrecognized result status",
		}, {
			name: "an unrecognized status with no response falls back to the stream",
			code: 0, res: &streamjson.Result{Status: "MAX_TURNS"},
			out: "streamed", wantState: StateFailed, wantResult: "streamed", wantPartial: true,
			wantErrSub: "unrecognized result status",
		},
		// --- clean exit, no terminal payload ------------------------------------
		{
			name: "a stream-json run with no payload was cut short",
			code: 0, out: "half an answer", args: streamArgs,
			wantState: StateDone, wantResult: "half an answer", wantPartial: true,
		}, {
			// A job an older agy-mcp wrote has a complete plain-text out and no
			// payload, because that build never wrote one. Calling it partial would
			// tell the caller a finished answer may be truncated for the whole TTL
			// after an upgrade.
			name: "a legacy job's plain-text output is complete",
			code: 0, out: "a complete v1 answer", args: legacyArgs,
			wantState: StateDone, wantResult: "a complete v1 answer",
		},
		// --- cancelled ----------------------------------------------------------
		{
			name: "a cancel with no payload carries the stream",
			code: jobstore.ExitSIGTERM, out: "got this far",
			wantState: StateCancelled, wantResult: "got this far", wantPartial: true,
		}, {
			// agy printed its result and then hung, so the run was killed with a
			// finished answer already on disk. Preferring the stream, or flagging the
			// payload because the state is not done, would both discard a complete
			// answer the caller asked for.
			name: "a cancel after a success payload is not partial",
			code: jobstore.ExitSIGTERM,
			res: &streamjson.Result{
				Status: streamjson.StatusSuccess, Response: "finished",
				ConversationID: "cid-cancelled", NumTurns: 4,
			},
			out: "streamed", wantState: StateCancelled, wantResult: "finished",
			// A run cut short still has a conversation worth continuing and
			// accounting worth reporting, and both come off the payload rather
			// than the exit code. Nothing pinned that on a non-zero exit, so
			// deleting the epilogue that carries them passed the whole suite.
			wantConvID: "cid-cancelled", wantTurns: 4,
		}, {
			// The bug: the payload branch never set Partial, so truncated text
			// arriving with an ERROR status was reported as a complete answer.
			name: "a cancel after an error payload is partial",
			code: jobstore.ExitSIGTERM, res: &streamjson.Result{Status: streamjson.StatusError, Response: "partial text"},
			out: "streamed", wantState: StateCancelled, wantResult: "partial text", wantPartial: true,
		}, {
			name: "a SIGINT is a cancel like a SIGTERM",
			code: jobstore.ExitSIGINT, out: "got this far",
			wantState: StateCancelled, wantResult: "got this far", wantPartial: true,
		}, {
			// The failed half of "a response agy marked SUCCESS is complete even
			// when the job then ended badly". Only the cancelled half was pinned,
			// so a rule that flagged every non-done state as partial passed the
			// whole suite.
			name: "a timeout after a success payload is not partial",
			code: jobstore.ExitTimeout, res: &streamjson.Result{Status: streamjson.StatusSuccess, Response: "finished"},
			out: "streamed", wantState: StateFailed, wantResult: "finished",
			wantErrSub: "timeout",
		}, {
			name: "a crash after a success payload is not partial",
			code: 1, res: &streamjson.Result{Status: streamjson.StatusSuccess, Response: "finished"},
			out: "streamed", wantState: StateFailed, wantResult: "finished",
			wantErrSub: "exit 1",
		},
		// --- timed out, crashed, spawn-failed -----------------------------------
		{
			name: "a timeout carries the stream",
			code: jobstore.ExitTimeout, out: "as far as I got",
			wantState: StateFailed, wantResult: "as far as I got", wantPartial: true,
			wantErrSub: "timeout",
		}, {
			// The bug: 127 never carried any text, though the branch beside it
			// argues that a crashed run loses its answer exactly as a timed-out one
			// does. A true spawn failure has no text, so nothing is lost there; a
			// genuine agy exiting 127 does.
			name: "a real agy 127 carries the stream",
			code: jobstore.ExitSpawnFail, out: "agy said this much",
			wantState: StateFailed, wantResult: "agy said this much", wantPartial: true,
			wantErrSub: "could not exec the agy binary",
		}, {
			name:      "a true spawn failure has nothing to carry",
			code:      jobstore.ExitSpawnFail,
			wantState: StateFailed, wantErrSub: "could not exec the agy binary",
		}, {
			name: "a crash carries the stream and the stderr tail",
			code: 1, out: "partial", errFile: "panic: boom",
			wantState: StateFailed, wantResult: "partial", wantPartial: true,
			wantErrSub: "panic: boom",
		}, {
			// A payload's own message outranks the stderr tail: agy reports failures
			// it survives in band, where the exit code is only ever 1.
			name: "a crash with an error payload uses its message and text",
			code: 1, res: &streamjson.Result{Status: streamjson.StatusError, Response: "partial text", Error: "model unavailable"},
			out: "streamed", errFile: "some stderr",
			wantState: StateFailed, wantResult: "partial text", wantPartial: true,
			wantErrSub: "model unavailable",
		},
	}
}

// TestStatusTerminalContractTableIsWellFormed checks the TABLE, not the code.
//
// The per-row assertions in TestStatusTerminalContract can only compare a row
// against itself, so a future row declaring an outcome the contract forbids
// would pass by being wrong twice: once in the want, once in the code it was
// written to match. This reads only the declared wants, so it is the one place
// that can catch that.
func TestStatusTerminalContractTableIsWellFormed(t *testing.T) {
	for _, tc := range terminalCases() {
		vouched := tc.res != nil && tc.res.Status == streamjson.StatusSuccess && tc.res.Response != ""
		// Ask the production predicate, not a copy of it: a validator that
		// classified a row differently from the code would pass exactly the rows
		// it exists to catch.
		legacy := tc.res == nil && tc.code == 0 && !argsSelectOutputFormat(tc.args)
		if tc.wantResult != "" && !tc.wantPartial && !vouched && !legacy {
			t.Errorf("row %q declares a result that is neither partial, nor vouched for by a SUCCESS payload response, nor a legacy job dir", tc.name)
		}
		if tc.wantResult == "" && tc.wantPartial {
			t.Errorf("row %q declares partial with no result, which says nothing to a caller", tc.name)
		}
	}
}

// stageTerminalJob writes the job directory a case describes and returns the
// status the manager derives from it.
func stageTerminalJob(t *testing.T, tc terminalCase) Status {
	t.Helper()
	m := newManager(t, managerOpts{})
	dir, err := m.store.Create(jobstore.Meta{
		ID: "j", StartedAt: time.Now(), BootID: readBootID(), Args: tc.args,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tc.res != nil {
		writeResultPayload(t, dir, *tc.res)
	}
	writeIfSet(t, jobstore.OutPath(dir), tc.out)
	writeIfSet(t, jobstore.ErrPath(dir), tc.errFile)
	if err := m.store.WriteExitCode("j", tc.code); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status("j")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// writeIfSet stages one job-dir file, leaving it absent when the case supplies
// no content. Absent and empty are different inputs here: an empty out file
// reads back as no text, while a missing one is what a run that never started
// leaves behind.
func writeIfSet(t *testing.T, path, content string) {
	t.Helper()
	if content == "" {
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertTerminalStatus(t *testing.T, st Status, tc terminalCase) {
	t.Helper()
	if st.State != tc.wantState {
		t.Errorf("state = %q, want %q", st.State, tc.wantState)
	}
	if st.Result != tc.wantResult {
		t.Errorf("result = %q, want %q", st.Result, tc.wantResult)
	}
	if st.Partial != tc.wantPartial {
		t.Errorf("partial = %v, want %v", st.Partial, tc.wantPartial)
	}
	if tc.wantErrSub == "" {
		if st.Error != "" {
			t.Errorf("error = %q, want none", st.Error)
		}
	} else if !strings.Contains(st.Error, tc.wantErrSub) {
		t.Errorf("error = %q, want it to mention %q", st.Error, tc.wantErrSub)
	}
	if tc.wantConvID != "" && st.ConversationID != tc.wantConvID {
		t.Errorf("conversation_id = %q, want %q carried off the payload", st.ConversationID, tc.wantConvID)
	}
	if tc.wantTurns != 0 && st.NumTurns != tc.wantTurns {
		t.Errorf("num_turns = %d, want %d carried off the payload", st.NumTurns, tc.wantTurns)
	}
}

func TestStatusTerminalContract(t *testing.T) {
	for _, tc := range terminalCases() {
		t.Run(tc.name, func(t *testing.T) {
			assertTerminalStatus(t, stageTerminalJob(t, tc), tc)
		})
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

// A job an older manager queued and a newer supervisor ran has no output-format
// flag in its persisted args, because the upgrade is applied to a copy at exec
// time. If its agy then emits nothing at all and exits 0, the args are the only
// thing left to go on and they say "legacy", so the run reads as a complete,
// empty answer: an outcome-less run reported as a verified one. The supervisor's
// spawn-time marker is what closes that, and this is the manager side of it.
func TestStatusUpgradedJobWithNoEventsIsPartial(t *testing.T) {
	m := newManager(t, managerOpts{})
	// The shape an older manager persisted: no output format anywhere in args.
	dir, err := m.store.Create(jobstore.Meta{
		ID: "j", StartedAt: time.Now(), BootID: readBootID(),
		Args: []string{"--dangerously-skip-permissions", "-p", "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// What the supervisor leaves before exec: a progress file with nothing in it
	// yet, because agy has not spoken.
	if werr := jobstore.WriteProgressDir(dir, jobstore.Progress{UpdatedAt: time.Now().UTC()}); werr != nil {
		t.Fatal(werr)
	}
	// agy exited 0 having emitted neither an event stream nor a result.
	if werr := m.store.WriteExitCode("j", 0); werr != nil {
		t.Fatal(werr)
	}

	st, err := m.Status("j")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateDone || st.Result != "" {
		t.Fatalf("status = %+v, want done with no result", st)
	}
	if !st.Partial {
		t.Fatal("an upgraded run that produced no outcome must not be reported as a verified empty answer")
	}
	if st.ConversationID != "" {
		t.Fatalf("conversation_id = %q, want empty: the marker carries no id", st.ConversationID)
	}
}

// A result payload that exists but cannot be decoded reports as absent, so the
// job lands in the no-payload branch with only the args and progress signals
// left. Both of those fail open, so an upgraded job dir (legacy args, no
// progress file) read as one an older build wrote and had its streamed text
// reported as a complete, verified answer. The file's existence is proof a
// stream was decoded, whatever state its contents are in.
func TestStatusCorruptResultPayloadIsStillAStreamJSONRun(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, err := m.store.Create(jobstore.Meta{
		ID: "j", StartedAt: time.Now(), BootID: readBootID(),
		Args: []string{"--dangerously-skip-permissions", "-p", "hi"}, // legacy: no output format
	})
	if err != nil {
		t.Fatal(err)
	}
	// Present but undecodable, and no progress file: the two other signals are
	// both silent.
	if werr := jobstore.WriteResultDir(dir, []byte("{not json")); werr != nil {
		t.Fatal(werr)
	}
	if werr := os.WriteFile(jobstore.OutPath(dir), []byte("half an answer"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if werr := m.store.WriteExitCode("j", 0); werr != nil {
		t.Fatal(werr)
	}

	st, err := m.Status("j")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateDone || st.Result != "half an answer" {
		t.Fatalf("status = %+v, want done with the streamed text", st)
	}
	if !st.Partial {
		t.Fatal("a run whose result payload is unreadable must not report its streamed text as a verified answer")
	}
}

// A job recovered without an exit-code sentinel takes its outcome from the
// terminal payload when the supervisor managed to write one before dying. The
// same Result/Partial contract applies: TestStatusTerminalContract covers the
// sentinel paths, this covers the recovery path that bypasses them.
func TestStatusRecoveredFromPayload(t *testing.T) {
	for _, tc := range []struct {
		name        string
		res         streamjson.Result
		out         string
		wantState   string
		wantResult  string
		wantPartial bool
	}{
		// A supervisor that died between writing the result and writing the
		// sentinel left a complete, trustworthy answer.
		{"success payload", streamjson.Result{Status: streamjson.StatusSuccess, Response: "complete"}, "streamed", StateDone, "complete", false},
		{"error payload keeps its text", streamjson.Result{Status: streamjson.StatusError, Response: "got this far"}, "", StateFailed, "got this far", true},
		{"a response with no status is unverified", streamjson.Result{Response: "an answer"}, "", StateDone, "an answer", true},
		{"an empty payload falls back to the stream", streamjson.Result{}, "streamed", StateFailed, "streamed", true},
		// The no-stream sibling, so the recovery path pins the indeterminate
		// verdict itself and not only the fallback that usually hides it.
		{"an empty payload with no stream is indeterminate", streamjson.Result{}, "", StateFailed, "", false},
		{"an unrecognized status", streamjson.Result{Status: "MAX_TURNS"}, "", StateFailed, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, managerOpts{})
			// A dead PID from a previous boot and no sentinel: the recovery path.
			dir, err := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), PID: 999999, BootID: "old-boot"})
			if err != nil {
				t.Fatal(err)
			}
			writeResultPayload(t, dir, tc.res)
			if tc.out != "" {
				if werr := os.WriteFile(jobstore.OutPath(dir), []byte(tc.out), 0o600); werr != nil {
					t.Fatal(werr)
				}
			}

			st, err := m.Status("j")
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.wantState || st.Result != tc.wantResult || st.Partial != tc.wantPartial {
				t.Fatalf("status = %+v, want state %q result %q partial %v",
					st, tc.wantState, tc.wantResult, tc.wantPartial)
			}
			// A failure must say why. The table this replaced asserted it and the
			// fold dropped it, leaving applyResult's two default messages
			// unpinned on the recovery path.
			if tc.wantState == StateFailed && st.Error == "" {
				t.Fatal("a failed recovery must carry an explanation")
			}
		})
	}
}
