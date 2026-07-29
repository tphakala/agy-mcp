//go:build linux || darwin

package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

func TestEnsureStreamJSON(t *testing.T) {
	// Args written by an older agy-mcp carry no output format; the supervisor
	// must add one rather than decode plain text as an event stream.
	got := ensureStreamJSON([]string{"--dangerously-skip-permissions", "-p", "hi"})
	if !slices.Contains(got, outputFormatFlag) || !slices.Contains(got, streamJSONFormat) {
		t.Fatalf("args = %q, want the output format appended", got)
	}

	// Args that already select a format are left exactly as they are, so a
	// future agy-mcp choosing a different one is not overridden.
	orig := []string{"--output-format", "stream-json", "-p", "hi"}
	if got := ensureStreamJSON(orig); !slices.Equal(got, orig) {
		t.Fatalf("args = %q, want them unchanged", got)
	}

	// The caller's slice comes from its Meta and must not be mutated.
	in := []string{"-p", "hi"}
	_ = ensureStreamJSON(in)
	if len(in) != 2 {
		t.Fatalf("input args were mutated: %q", in)
	}
}

// writeJob stages a job dir whose agy is the given script.
func writeJob(t *testing.T, agyPath string, args []string) string {
	t.Helper()
	dir := t.TempDir()
	meta := jobstore.Meta{
		ID: filepath.Base(dir), AgyPath: agyPath, Args: args,
		Cwd: dir, StartedAt: time.Now(), Timeout: 30 * time.Second,
	}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobstore.MetaPath(dir), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// An in-place binary upgrade pairs an old manager's job (args with no output
// format) with this new supervisor. The run must still produce a real result
// rather than an empty one, which is what happens if plain text reaches the
// stream decoder.
func TestRunAddsStreamJSONForLegacyArgs(t *testing.T) {
	// The script only emits the event stream when asked for it, exactly as agy
	// behaves: without the flag it would print prose the decoder cannot read.
	agy := writeScript(t, `
for a in "$@"; do
  if [ "$a" = "stream-json" ]; then
    printf '%s\n' '{"event":"init","conversation_id":"c-legacy"}'
    printf '%s\n' '{"event":"result","result":{"conversation_id":"c-legacy","status":"SUCCESS","response":"upgraded"}}'
    exit 0
  fi
done
printf '%s\n' 'plain text answer no decoder can read'
exit 0
`)
	dir := writeJob(t, agy, []string{"-p", "hi"})

	if err := run(dir, 100*time.Millisecond); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, rerr := jobstore.ReadResultDir(dir)
	if rerr != nil || b == nil {
		t.Fatal("no result payload: the legacy args were not upgraded to stream-json")
	}
	var res streamjson.Result
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if res.Response != "upgraded" {
		t.Fatalf("response = %q, want the decoded answer", res.Response)
	}
	if p, ok := jobstore.ReadProgressDir(dir); !ok || p.ConversationID != "c-legacy" {
		t.Fatalf("progress = %+v, want the conversation recorded", p)
	}
}

// A descendant that escapes the process group holds the inherited stdout
// descriptor open, so the stream never reaches EOF. The supervisor must still
// reap agy and write the exit-code sentinel instead of stranding the job in
// `running` forever.
func TestRunWritesSentinelDespiteDetachedDescendant(t *testing.T) {
	// setsid puts the sleeper in its own session, so the supervisor's group kill
	// cannot reach it. It inherits stdout and holds the pipe open.
	agy := writeScript(t, `
printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"done"}}'
setsid sleep 30 &
exit 0
`)
	dir := writeJob(t, agy, []string{"--output-format", "stream-json", "-p", "hi"})

	done := make(chan error, 1)
	go func() { done <- run(dir, 100*time.Millisecond) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(drainGrace + 20*time.Second):
		t.Fatal("run never returned: a detached descendant held the stream open")
	}

	code, err := os.ReadFile(jobstore.ExitCodePath(dir))
	if err != nil {
		t.Fatalf("no exit-code sentinel written: %v", err)
	}
	if string(code) != "0" {
		t.Fatalf("exit code = %q, want 0", code)
	}
	// The result decoded before the descendant stalled the stream is kept.
	if b, _ := jobstore.ReadResultDir(dir); b == nil {
		t.Fatal("result payload lost when the drain was released")
	}
}

// The manager treats a visible exit-code sentinel as proof that nothing is still
// writing the job dir: it is why the garbage collector may remove a terminal job
// immediately, and why recoverInterrupted trusts a result found without a
// sentinel. Nothing pinned that ordering, so moving the result write after the
// sentinel kept the whole suite green.
func TestRunWritesResultBeforeExitCodeSentinel(t *testing.T) {
	agy := writeScript(t, `
printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"ordered"}}'
exit 0
`)
	dir := writeJob(t, agy, []string{"--output-format", "stream-json", "-p", "hi"})

	if err := run(dir, 100*time.Millisecond); err != nil {
		t.Fatalf("run: %v", err)
	}
	resInfo, err := os.Stat(filepath.Join(dir, jobstore.ResultFile))
	if err != nil {
		t.Fatalf("result.json: %v", err)
	}
	sentInfo, err := os.Stat(jobstore.ExitCodePath(dir))
	if err != nil {
		t.Fatalf("exit_code: %v", err)
	}
	if resInfo.ModTime().After(sentInfo.ModTime()) {
		t.Fatalf("result.json (%s) was written AFTER the exit-code sentinel (%s); a manager that sees the sentinel would read an incomplete job dir",
			resInfo.ModTime(), sentInfo.ModTime())
	}
}
