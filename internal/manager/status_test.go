package manager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
	if st.FailureReason != ReasonInterrupted {
		t.Fatalf("failure_reason = %q, want %q", st.FailureReason, ReasonInterrupted)
	}
	if st.Partial {
		t.Fatalf("a no-output interruption is not a partial result: %+v", st)
	}
}

// TestClassifyAgyError pins the quota/rate-limit matcher: the wording agy has
// been seen to relay and the common provider spellings map to a retryable
// ReasonQuotaExhausted, while an ordinary agy error stays ReasonAgyError. The
// negatives matter as much as the positives: a hard error misread as transient
// would tell a caller to wait out a wall that never clears.
func TestClassifyAgyError(t *testing.T) {
	// Each positive is written to match exactly ONE isQuotaError substring, so
	// deleting any single matcher turns exactly one row red. A string matching two
	// matchers at once (say "429 ... rate limit") would leave a deleted matcher
	// silently covered by its neighbour.
	quota := []string{
		"Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 21m50s.", // quota
		"you have hit your rate limit for this model",                                                           // rate limit (space)
		"provider returned a rate-limit response",                                                               // rate-limit (hyphen)
		"the model is resource exhausted right now",                                                             // resource exhausted (space)
		"grpc status resource_exhausted on this request",                                                        // resource_exhausted (underscore)
		"grpc code ResourceExhausted returned",                                                                  // resourceexhausted (camelCase, lowercased)
		"got HTTP 429 from the provider",                                                                        // http 429 (status line, not a bare 429)
		"provider says: Too Many Requests",                                                                      // too many requests
		// Contains "disk quota" (which suppresses the "quota" token) yet still
		// carries a real "rate limit" signal, so it stays retryable. This pins the
		// disk-quota exclusion to the quota token alone: a whole-function early
		// return on "disk quota" would misclassify this as a hard error.
		"disk quota note aside, you hit a rate limit; retry later",
	}
	for _, msg := range quota {
		if got := classifyAgyError(msg); got != ReasonQuotaExhausted {
			t.Errorf("classifyAgyError(%q) = %q, want %q", msg, got, ReasonQuotaExhausted)
		}
	}
	other := []string{
		"model unavailable",
		"panic: nil pointer dereference",
		"context deadline exceeded",
		"agy reported an error without a message",
		"", // an ERROR payload with no message must not read as a quota wall
		// EDQUOT contains "quota" but is a hard OS storage error, not a provider
		// wall; it must fall to ReasonAgyError so the caller is not told to wait
		// for a reset that never comes.
		"write /var/data/out.tmp: disk quota exceeded",
	}
	for _, msg := range other {
		if got := classifyAgyError(msg); got != ReasonAgyError {
			t.Errorf("classifyAgyError(%q) = %q, want %q", msg, got, ReasonAgyError)
		}
	}
}

// TestErrorSummaryTruncatesOnUTF8Boundary: when the trailing stderr is larger
// than errTailBytes and the cut falls mid-rune, the reported error is advanced
// to a valid UTF-8 boundary rather than emitting a split multi-byte rune.
func TestErrorSummaryTruncatesOnUTF8Boundary(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir := createJob(t, m, "j")
	// 3-byte runes so the last errTailBytes window starts mid-rune (2000 % 3 != 0).
	content := strings.Repeat("€", 1000) // 3000 bytes
	if err := os.WriteFile(jobstore.ErrPath(dir), []byte(content), 0o644); err != nil {
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

// errorSummary must tell three cases apart, because this string is all a caller
// sees for a non-zero exit with no error message from a result payload (no
// payload, or a payload whose Error was empty). The empty-stderr case is
// the one that used to render as a dangling "exit N:", which reads as a message
// that got cut off rather than as the absence of one. MEASURED against agy
// 1.1.22: a run terminating non-zero with nothing on stderr does occur, and its
// status Error was exactly "exit 1:".
func TestErrorSummaryDistinguishesEmptyStderr(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stderr     string
		writeIt    bool
		errIsDir   bool
		code       int
		want       string
		wantPrefix bool
	}{
		{name: "stderr with content", stderr: "boom", writeIt: true, code: 1, want: "exit 1: boom"},
		{name: "trailing whitespace trimmed", stderr: "boom\n\n", writeIt: true, code: 2, want: "exit 2: boom"},
		// Leading whitespace is part of the message and must survive: cleanTail
		// trims only the trailing end. This pins the parity a TrimRightFunc ->
		// TrimSpace change would break.
		{name: "leading whitespace preserved", stderr: "  boom  \n", writeIt: true, code: 1, want: "exit 1:   boom"},
		{name: "empty stderr file", stderr: "", writeIt: true, code: 1, want: "exit 1 (no stderr output)"},
		{name: "whitespace-only stderr", stderr: "  \t\n ", writeIt: true, code: 3, want: "exit 3 (no stderr output)"},
		{name: "no stderr file at all", writeIt: false, code: 4, want: "exit 4 (no stderr output)"},
		// A directory at the stderr path makes tailFile's read fail, which is the
		// only way to reach errorSummary's <stderr unavailable> branch. POSIX only:
		// see the skip below.
		{name: "unreadable stderr", errIsDir: true, code: 5, want: "exit 5: <stderr unavailable:", wantPrefix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, managerOpts{})
			dir := createJob(t, m, "j")
			switch {
			case tc.errIsDir:
				// A directory reads as an error on POSIX, but not on Windows: there a
				// directory's Size() is 0, so tailFile sizes a zero-length buffer and
				// (*os.File).ReadAt returns (0, nil) without ever reading, leaving
				// tail empty and no error. Skipping keeps this row honest rather than
				// asserting a branch the platform cannot reach. NOT MEASURED on
				// Windows; derived from tailFile's buffer sizing and ReadAt's
				// len(b) > 0 loop.
				if runtime.GOOS == "windows" {
					t.Skip("a directory does not make tailFile's read fail on Windows")
				}
				if err := os.Mkdir(jobstore.ErrPath(dir), 0o755); err != nil {
					t.Fatal(err)
				}
			case tc.writeIt:
				if err := os.WriteFile(jobstore.ErrPath(dir), []byte(tc.stderr), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := errorSummary(dir, tc.code)
			if tc.wantPrefix {
				if !strings.HasPrefix(got, tc.want) {
					t.Errorf("errorSummary = %q, want prefix %q", got, tc.want)
				}
				return
			}
			if got != tc.want {
				t.Errorf("errorSummary = %q, want %q", got, tc.want)
			}
		})
	}
}

// The same absence, seen through Status, which is where a caller actually meets
// it. Pinned separately so the message cannot regress only on the path that
// matters while the unit test above still passes.
func TestStatusFailedWithMissingStderrNamesTheAbsence(t *testing.T) {
	m := newManager(t, managerOpts{})
	createJob(t, m, "j")
	_ = m.store.WriteExitCode("j", 1)

	st, err := m.Status("j")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != StateFailed {
		t.Fatalf("state = %q, want failed", st.State)
	}
	if st.Error != "exit 1 (no stderr output)" {
		t.Fatalf("error = %q, want the absence named rather than a dangling colon", st.Error)
	}
}

func TestStatusDone(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir := createJob(t, m, "j")
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
	wantReason  string // FailureReason; required on every failed row, forbidden elsewhere
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
			wantErrSub: "model unavailable", wantReason: ReasonAgyError,
		}, {
			name: "error payload with no text falls back to the stream",
			code: 0, res: &streamjson.Result{Status: streamjson.StatusError, Error: "model unavailable"},
			out: "streamed", wantState: StateFailed, wantResult: "streamed", wantPartial: true,
			wantErrSub: "model unavailable", wantReason: ReasonAgyError,
		}, {
			name: "error payload with no message still explains itself",
			code: 0, res: &streamjson.Result{Status: streamjson.StatusError},
			wantState: StateFailed, wantErrSub: "without a message", wantReason: ReasonAgyError,
		}, {
			// A quota or rate-limit wall arrives as an ERROR payload like any other,
			// but is transient: it must be told apart from a hard error so a caller
			// can wait out the reset rather than give up. The reset time stays in the
			// message; the reason is what makes it branchable.
			name: "a quota wall is classified as retryable, not a hard error",
			code: 0, res: &streamjson.Result{
				Status: streamjson.StatusError,
				Error:  "Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 21m50s.",
			},
			wantState: StateFailed, wantErrSub: "Resets in 21m50s", wantReason: ReasonQuotaExhausted,
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
			wantState: StateFailed, wantErrSub: "no status and no response", wantReason: ReasonAgyError,
		}, {
			name: "an indeterminate payload still falls back to the stream",
			code: 0, res: &streamjson.Result{},
			out: "streamed", wantState: StateFailed, wantResult: "streamed", wantPartial: true,
			wantErrSub: "no status and no response", wantReason: ReasonAgyError,
		}, {
			name: "an unrecognized status keeps its response",
			code: 0, res: &streamjson.Result{Status: "MAX_TURNS", Response: "as far as I got"},
			wantState: StateFailed, wantResult: "as far as I got", wantPartial: true,
			wantErrSub: "unrecognized result status", wantReason: ReasonAgyError,
		}, {
			// The no-text sibling of the row above. Every other payload status has
			// one, and the asymmetry is what let this case lose its coverage when
			// four older tests were folded into this table.
			name: "an unrecognized status with no response",
			code: 0, res: &streamjson.Result{Status: "MAX_TURNS"},
			wantState: StateFailed, wantErrSub: "unrecognized result status", wantReason: ReasonAgyError,
		}, {
			name: "an unrecognized status with no response falls back to the stream",
			code: 0, res: &streamjson.Result{Status: "MAX_TURNS"},
			out: "streamed", wantState: StateFailed, wantResult: "streamed", wantPartial: true,
			wantErrSub: "unrecognized result status", wantReason: ReasonAgyError,
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
			wantErrSub: "timeout", wantReason: ReasonTimeout,
		}, {
			name: "a crash after a success payload is not partial",
			code: 1, res: &streamjson.Result{Status: streamjson.StatusSuccess, Response: "finished"},
			out: "streamed", wantState: StateFailed, wantResult: "finished",
			wantErrSub: "exit 1", wantReason: ReasonAgyError,
		},
		// --- timed out, crashed, spawn-failed -----------------------------------
		{
			name: "a timeout carries the stream",
			code: jobstore.ExitTimeout, out: "as far as I got",
			wantState: StateFailed, wantResult: "as far as I got", wantPartial: true,
			wantErrSub: "timeout", wantReason: ReasonTimeout,
		}, {
			// The bug: 127 never carried any text, though the branch beside it
			// argues that a crashed run loses its answer exactly as a timed-out one
			// does. A true spawn failure has no text, so nothing is lost there; a
			// genuine agy exiting 127 does.
			name: "a real agy 127 carries the stream",
			code: jobstore.ExitSpawnFail, out: "agy said this much",
			wantState: StateFailed, wantResult: "agy said this much", wantPartial: true,
			wantErrSub: "could not exec the agy binary", wantReason: ReasonSpawnFailed,
		}, {
			name:      "a true spawn failure has nothing to carry",
			code:      jobstore.ExitSpawnFail,
			wantState: StateFailed, wantErrSub: "could not exec the agy binary", wantReason: ReasonSpawnFailed,
		}, {
			name: "a crash carries the stream and the stderr tail",
			code: 1, out: "partial", errFile: "panic: boom",
			wantState: StateFailed, wantResult: "partial", wantPartial: true,
			wantErrSub: "panic: boom", wantReason: ReasonAgyError,
		}, {
			// A non-zero exit whose stderr is a quota wall (agy usually reports one in
			// its payload, but may exit non-zero with it only on stderr) is still
			// classified as retryable off the stderr tail, not flatly as agy_error.
			name: "a non-zero exit with a quota wall on stderr is retryable",
			code: 1, errFile: "Error: 429 Too Many Requests: rate limit exceeded",
			wantState: StateFailed, wantErrSub: "rate limit", wantReason: ReasonQuotaExhausted,
		}, {
			// A payload's own message outranks the stderr tail: agy reports failures
			// it survives in band, where the exit code is only ever 1.
			name: "a crash with an error payload uses its message and text",
			code: 1, res: &streamjson.Result{Status: streamjson.StatusError, Response: "partial text", Error: "model unavailable"},
			out: "streamed", errFile: "some stderr",
			wantState: StateFailed, wantResult: "partial text", wantPartial: true,
			wantErrSub: "model unavailable", wantReason: ReasonAgyError,
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
		// FailureReason is set with, and only with, StateFailed: a failed row that
		// declares no reason would leave the field blank exactly where a caller
		// reaches for it, and a done or cancelled row that declares one asserts a
		// reason the code must never set.
		if tc.wantState == StateFailed && tc.wantReason == "" {
			t.Errorf("row %q is a failure but declares no failure_reason", tc.name)
		}
		if tc.wantState != StateFailed && tc.wantReason != "" {
			t.Errorf("row %q is not a failure yet declares failure_reason %q", tc.name, tc.wantReason)
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
	if st.FailureReason != tc.wantReason {
		t.Errorf("failure_reason = %q, want %q", st.FailureReason, tc.wantReason)
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

func TestArgsSelectJSONSchemaAcceptsBothFlagSpellings(t *testing.T) {
	for _, args := range [][]string{
		{jsonSchemaFlag, `{"type":"object"}`},
		{jsonSchemaFlag + `={"type":"object"}`},
	} {
		if !argsSelectJSONSchema(args) {
			t.Fatalf("argsSelectJSONSchema(%q) = false, want true", args)
		}
	}
	if argsSelectJSONSchema([]string{"--not-json-schema", "x"}) {
		t.Fatal("unrelated flag was classified as json-schema")
	}
	// The prompt value after -p is caller free text, not an option: a prompt that
	// is (or begins with) the flag must NOT be read as selecting a schema run,
	// which would wrongly fail the run's normal response for lacking structured_output.
	for _, args := range [][]string{
		{promptFlag, jsonSchemaFlag},
		{promptFlag, jsonSchemaFlag + "={}"},
		{outputFormatFlag, streamJSONFormat, promptFlag, jsonSchemaFlag},
	} {
		if argsSelectJSONSchema(args) {
			t.Fatalf("argsSelectJSONSchema(%q) = true, want false: a prompt value must not be classified as an option", args)
		}
	}
	// A genuine --json-schema option before the prompt is still detected even when
	// the prompt itself happens to look like the flag.
	if !argsSelectJSONSchema([]string{jsonSchemaFlag, "{}", promptFlag, jsonSchemaFlag}) {
		t.Fatal("real --json-schema option before the prompt was not detected")
	}
}

func TestStatusJSONSchemaResultSelection(t *testing.T) {
	schemaArgs := []string{outputFormatFlag, streamJSONFormat, jsonSchemaFlag, `{"type":"object"}`, "-p", "hi"}
	responseWithToolMetadata := `{"business":"ok","toolAction":"Finishing task","toolSummary":"Task completion"}`

	for _, tc := range []terminalCase{
		{
			name: "schema success returns structured output instead of response metadata",
			code: 0,
			args: schemaArgs,
			res: &streamjson.Result{
				Status:           streamjson.StatusSuccess,
				Response:         responseWithToolMetadata,
				StructuredOutput: json.RawMessage(`{"business":"ok"}`),
			},
			wantState: StateDone, wantResult: `{"business":"ok"}`,
		}, {
			name:      "schema success without structured output fails closed",
			code:      0,
			args:      schemaArgs,
			res:       &streamjson.Result{Status: streamjson.StatusSuccess, Response: responseWithToolMetadata},
			wantState: StateFailed, wantResult: responseWithToolMetadata, wantPartial: true,
			wantErrSub: "without structured_output", wantReason: ReasonAgyError,
		}, {
			name: "non schema success keeps response behavior",
			code: 0,
			res: &streamjson.Result{
				Status:           streamjson.StatusSuccess,
				Response:         responseWithToolMetadata,
				StructuredOutput: json.RawMessage(`{"business":"ok"}`),
			},
			wantState: StateDone, wantResult: responseWithToolMetadata,
		}, {
			name: "schema clean exit without terminal event fails closed",
			code: 0, args: schemaArgs, out: "streamed diagnostic",
			wantState: StateFailed, wantResult: "streamed diagnostic", wantPartial: true,
			wantErrSub: "without a terminal structured_output", wantReason: ReasonAgyError,
		}, {
			name: "cancel after schema success keeps complete structured output",
			code: jobstore.ExitSIGTERM, args: schemaArgs,
			res: &streamjson.Result{
				Status:           streamjson.StatusSuccess,
				Response:         responseWithToolMetadata,
				StructuredOutput: json.RawMessage(`{"business":"ok"}`),
			},
			wantState: StateCancelled, wantResult: `{"business":"ok"}`,
		}, {
			name: "timeout after schema success keeps complete structured output",
			code: jobstore.ExitTimeout, args: schemaArgs,
			res: &streamjson.Result{
				Status:           streamjson.StatusSuccess,
				Response:         responseWithToolMetadata,
				StructuredOutput: json.RawMessage(`{"business":"ok"}`),
			},
			wantState: StateFailed, wantResult: `{"business":"ok"}`,
			wantErrSub: "timeout", wantReason: ReasonTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := stageTerminalJob(t, tc)
			if st.State != tc.wantState || st.Result != tc.wantResult || st.Partial != tc.wantPartial {
				t.Fatalf("status = %+v, want state %q result %q partial %v", st, tc.wantState, tc.wantResult, tc.wantPartial)
			}
			if tc.wantErrSub != "" && !strings.Contains(st.Error, tc.wantErrSub) {
				t.Fatalf("error = %q, want substring %q", st.Error, tc.wantErrSub)
			}
			if st.FailureReason != tc.wantReason {
				t.Fatalf("failure_reason = %q, want %q", st.FailureReason, tc.wantReason)
			}
		})
	}

	for _, raw := range []string{`{"k":1}`, `[1,2]`, `"scalar"`, `null`} {
		t.Run("shape "+raw, func(t *testing.T) {
			st := stageTerminalJob(t, terminalCase{
				name: "shape", code: 0, args: schemaArgs,
				res: &streamjson.Result{Status: streamjson.StatusSuccess, StructuredOutput: json.RawMessage(raw)},
			})
			if st.State != StateDone || st.Result != raw || st.Partial {
				t.Fatalf("status = %+v, want done result %q partial false", st, raw)
			}
		})
	}
}

func TestStatusHistoricalSchemaJobWithoutStructuredOutputFailsClosed(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir, err := m.store.Create(jobstore.Meta{
		ID: "old-schema", StartedAt: time.Now(), BootID: readBootID(),
		Args: []string{outputFormatFlag, streamJSONFormat, jsonSchemaFlag, `{"type":"object"}`, "-p", "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeResultPayload(t, dir, streamjson.Result{Status: streamjson.StatusSuccess, Response: `{"legacy":"response"}`})
	if err := m.store.WriteExitCode("old-schema", 0); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status("old-schema")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateFailed || st.FailureReason != ReasonAgyError || !st.Partial {
		t.Fatalf("historical schema status = %+v, want failed/agy_error/partial", st)
	}
	if st.Result != `{"legacy":"response"}` || !strings.Contains(st.Error, "without structured_output") {
		t.Fatalf("historical schema diagnostic = %+v", st)
	}
}

// A cancelled job carries no result, but it does carry the conversation, so the
// thread it started can still be continued.
func TestStatusCancelledKeepsConversationID(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir := createJob(t, m, "j")
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
	dir := createJob(t, m, "j")
	_ = os.WriteFile(jobstore.ErrPath(dir), []byte("boom"), 0o644)
	_ = m.store.WriteExitCode("j", 5)

	st, _ := m.Status("j")
	if st.State != StateFailed || st.Error == "" {
		t.Fatalf("status = %+v", st)
	}
}

func TestStatusTimedOut(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir := createJob(t, m, "j")
	_ = os.WriteFile(jobstore.ErrPath(dir), []byte("partial"), 0o644)
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
	dir := createJob(t, m, "j")
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = m.store.WriteExitCode("j", 0)

	st, _ := m.Status("j")
	if st.State != StateFailed || st.Error == "" {
		t.Fatalf("status = %+v, want failed when the output file cannot be read", st)
	}
	// An unreadable output is not agy's failure and cannot be classified further,
	// so it is ReasonUnknown; pin it so the reason cannot silently regress to
	// ReasonAgyError (which would tell a caller agy itself errored).
	if st.FailureReason != ReasonUnknown {
		t.Fatalf("failure_reason = %q, want %q for an unreadable output", st.FailureReason, ReasonUnknown)
	}
}

// TestStatusInterruptedOutputUnreadable exercises the recovery path (dead PID,
// no exit-code sentinel) when the output file also cannot be read: the job is a
// failure this build cannot describe from agy's own account, so it is
// ReasonUnknown. This is the status.go recoverInterrupted read-error branch,
// which no other test reaches.
func TestStatusInterruptedOutputUnreadable(t *testing.T) {
	m := newManager(t, managerOpts{})
	// BootID differs from current -> the recorded PID is from a previous boot, so
	// the process cannot be alive and there is no sentinel: the recovery path.
	dir, _ := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), PID: 999999, BootID: "old-boot"})
	// A directory at the out path makes readFile fail, unlike a missing file
	// (which reads as an empty, clean interruption).
	if err := os.Mkdir(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}

	st, _ := m.Status("j")
	if st.State != StateFailed || st.Error == "" {
		t.Fatalf("status = %+v, want failed when the interrupted output cannot be read", st)
	}
	if st.FailureReason != ReasonUnknown {
		t.Fatalf("failure_reason = %q, want %q for an unreadable interrupted output", st.FailureReason, ReasonUnknown)
	}
}

// TestStatusSpawnFail: ExitSpawnFail (127) with no stderr (a true spawn failure)
// gets a dedicated message instead of a bare "exit 127:".
func TestStatusSpawnFail(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir := createJob(t, m, "j")
	_ = os.WriteFile(jobstore.ErrPath(dir), []byte(""), 0o644)
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
// spawnFailMessage names an unreadable stderr rather than dropping the error,
// the same way errorSummary does. Without this the branch has no coverage at
// all, and removing it leaves the whole package green.
//
// POSIX only, for the reason TestErrorSummaryDistinguishesEmptyStderr's
// unreadable row is: on Windows a directory's Size() is 0, so tailFile sizes a
// zero-length buffer and ReadAt returns (0, nil) without reading, so the read
// never fails and there is no error to name.
func TestStatusSpawnFailNamesUnreadableStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory does not make tailFile's read fail on Windows")
	}
	m := newManager(t, managerOpts{})
	dir := createJob(t, m, "j")
	if err := os.Mkdir(jobstore.ErrPath(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.store.WriteExitCode("j", jobstore.ExitSpawnFail); err != nil {
		t.Fatalf("WriteExitCode: %v", err)
	}

	st, err := m.Status("j")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(st.Error, "stderr unavailable:") {
		t.Fatalf("error = %q, want the unreadable stderr named", st.Error)
	}
	// The 127 explanation must survive alongside it, so the added clause
	// supplements the message rather than replacing it.
	if !strings.Contains(st.Error, "could not exec the agy binary") {
		t.Fatalf("error = %q, want the spawn-failure explanation kept", st.Error)
	}
}

func TestStatusExit127SurfacesStderr(t *testing.T) {
	m := newManager(t, managerOpts{})
	dir := createJob(t, m, "j")
	_ = os.WriteFile(jobstore.ErrPath(dir), []byte("agy: internal tool not found\n"), 0o644)
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
	dir := createJob(t, m, "j")
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

// TestStatusEchoesResolvedModel: Status echoes the resolved model persisted to
// meta (issue #138 item 2), so a caller can see which model actually ran. It is
// set at construction, before the terminal-vs-running branch, so it rides every
// state; an empty meta.Model stays empty (the server pinned no model).
func TestStatusEchoesResolvedModel(t *testing.T) {
	boot := readBootID()
	for _, tc := range []struct {
		name      string
		metaModel string
		pid       int
		bootID    string
		done      bool
		want      string
	}{
		{"done state carries the model", "gemini-3.1-pro-high", 0, boot, true, "gemini-3.1-pro-high"},
		{"interrupted state carries the model too", "gemini-3.1-pro-high", 999999, "old-boot", false, "gemini-3.1-pro-high"},
		{"no pinned model stays empty", "", 0, boot, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, managerOpts{})
			dir, err := m.store.Create(jobstore.Meta{ID: "j", StartedAt: time.Now(), PID: tc.pid, BootID: tc.bootID, Model: tc.metaModel})
			if err != nil {
				t.Fatal(err)
			}
			if tc.done {
				writeResultPayload(t, dir, streamjson.Result{Status: streamjson.StatusSuccess, Response: "x"})
				_ = m.store.WriteExitCode("j", 0)
			}
			st, err := m.Status("j")
			if err != nil {
				t.Fatal(err)
			}
			if st.Model != tc.want {
				t.Errorf("st.Model = %q, want %q (state %q)", st.Model, tc.want, st.State)
			}
		})
	}
}
