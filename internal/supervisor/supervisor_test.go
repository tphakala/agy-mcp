//go:build !windows

package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

func writeMeta(t *testing.T, dir string, m jobstore.Meta) {
	t.Helper()
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveExitCode(t *testing.T) {
	cases := []struct {
		name       string
		raw        int
		waitFailed bool
		timedOut   bool
		cancelled  bool
		want       int
	}{
		{"natural success is untouched", 0, false, false, false, 0},
		{"natural failure is untouched", 5, true, false, false, 5},
		{"crash with no supervisor termination stays a failure", 128 + 11, true, false, false, 128 + 11},
		{"timeout that died by SIGTERM reports timeout", jobstore.ExitSIGTERM, true, true, false, jobstore.ExitTimeout},
		{"timeout that escalated to SIGKILL reports timeout", 128 + 9, true, true, false, jobstore.ExitTimeout},
		{"cancel that died by SIGTERM reports cancel", jobstore.ExitSIGTERM, true, false, true, jobstore.ExitSIGTERM},
		{"cancel that escalated to SIGKILL still reports cancel", 128 + 9, true, false, true, jobstore.ExitSIGTERM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveExitCode(tc.raw, tc.waitFailed, tc.timedOut, tc.cancelled); got != tc.want {
				t.Errorf("resolveExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEffectiveTimeout(t *testing.T) {
	if got := effectiveTimeout(0); got != fallbackTimeout {
		t.Errorf("effectiveTimeout(0) = %v, want fallback %v", got, fallbackTimeout)
	}
	if got := effectiveTimeout(-5 * time.Second); got != fallbackTimeout {
		t.Errorf("effectiveTimeout(negative) = %v, want fallback %v", got, fallbackTimeout)
	}
	if got := effectiveTimeout(5 * time.Minute); got != 5*time.Minute {
		t.Errorf("effectiveTimeout(5m) = %v, want passthrough", got)
	}
}

func TestSupervisorCapturesOutputAndSentinel(t *testing.T) {
	dir := t.TempDir()
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "review text", Stderr: "warn", Exit: 0})
	writeMeta(t, dir, jobstore.Meta{ID: "j", AgyPath: agy, Args: []string{"-p", "x"}, StartedAt: time.Now()})

	if err := Run(dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "out"))
	if strings.TrimSpace(string(out)) != "review text" {
		t.Fatalf("out = %q", out)
	}
	// The configured stderr must actually be captured to the err file, not just
	// configured and ignored. Use the shared job-dir path contract.
	errOut, err := os.ReadFile(jobstore.ErrPath(dir))
	if err != nil {
		t.Fatalf("read err file: %v", err)
	}
	if strings.TrimSpace(string(errOut)) != "warn" {
		t.Fatalf("err = %q, want %q", errOut, "warn")
	}
	code, err := os.ReadFile(filepath.Join(dir, "exit_code"))
	if err != nil || strings.TrimSpace(string(code)) != "0" {
		t.Fatalf("exit_code = %q, err=%v", code, err)
	}
}

// End-to-end counterpart to TestConsumeStreamAccumulatesChunksWithinOneStep:
// the fake emits the response as several ACTIVE deltas plus a DONE tail on one
// step, the way agy streams a long response, and the supervisor must land the
// whole thing in the out file. The direct consumeStream test pins the decoder;
// this pins the path a real run takes, including the fake that stands in for
// agy, so the two cannot drift apart.
func TestSupervisorAccumulatesChunkedDeltas(t *testing.T) {
	dir := t.TempDir()
	// Long enough that chunking is meaningful, and non-ASCII so a chunk boundary
	// landing inside a multi-byte rune would corrupt the reassembly.
	const answer = "the quick brown fox jumps over the lazy dog, hyvää yötä, 你好世界"
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: answer, StreamChunks: 7, Exit: 0})
	writeMeta(t, dir, jobstore.Meta{ID: "j", AgyPath: agy, Args: []string{"-p", "x"}, StartedAt: time.Now()})

	if err := Run(dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out, err := os.ReadFile(jobstore.OutPath(dir))
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if string(out) != answer {
		t.Fatalf("out = %q, want the concatenation of every delta %q", out, answer)
	}
	// The terminal result carries the same text, so out matching it is the
	// property the manager relies on when it falls back to the streamed text.
	res, err := jobstore.ReadResultDir(dir)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var got streamjson.Result
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.Response != string(out) {
		t.Fatalf("result response %q != streamed out %q", got.Response, out)
	}
}

func TestSupervisorRecordsNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: "fail", Exit: 7})
	writeMeta(t, dir, jobstore.Meta{ID: "j", AgyPath: agy, StartedAt: time.Now()})
	if err := Run(dir); err != nil {
		t.Fatalf("Run should not error on non-zero agy exit: %v", err)
	}
	code, _ := os.ReadFile(filepath.Join(dir, "exit_code"))
	if strings.TrimSpace(string(code)) != "7" {
		t.Fatalf("exit_code = %q, want 7", code)
	}
}

func TestSupervisorHardTimeoutKillsAgy(t *testing.T) {
	dir := t.TempDir()
	// A fake agy that would sleep far longer than the hard timeout.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "should not finish", Sleep: 30 * time.Second})
	writeMeta(t, dir, jobstore.Meta{
		ID: "j", AgyPath: agy, Args: []string{"-p", "x"},
		StartedAt: time.Now(), Timeout: 500 * time.Millisecond,
	})

	start := time.Now()
	if err := Run(dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run took %v; the hard timeout did not kill agy", elapsed)
	}
	code, _ := os.ReadFile(filepath.Join(dir, "exit_code"))
	if got := strings.TrimSpace(string(code)); got != strconv.Itoa(jobstore.ExitTimeout) {
		t.Fatalf("exit_code = %q, want %d (timeout)", got, jobstore.ExitTimeout)
	}
}
