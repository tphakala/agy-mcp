package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// FakeSupervisor configures a stand-in supervisor script for tests.
//
// The generated script mimics `agy-mcp run-job <jobdir>`: it takes the job
// directory as $2, writes the job's out (and err/exit_code) files there, and,
// on Linux only, sets its comm to the script basename so the liveness comm
// fallback sees the same value the real supervisor would report. On macOS,
// identity is pinned by process start time instead, so the fake — a real
// spawned process — needs no comm trick there.
type FakeSupervisor struct {
	// AgyPath, when set, makes the script run that (fake) agy binary with
	// `-p x`, capturing stderr to <dir>/err and recording agy's real exit code.
	// Mutually exclusive with Out/Exit.
	//
	// agy's stdout is discarded rather than copied to <dir>/out, because it is a
	// stream-json event stream and the real supervisor decodes it instead of
	// copying it. Set Agy to the same config to have the decoded job-dir files
	// (out, progress.json, result.json) rendered in Go and written the way the
	// real supervisor would write them.
	AgyPath string
	// Agy, when set, supplies the stream-derived job-dir files for an AgyPath
	// run: the response text in out and the progress file before agy starts (the
	// real supervisor writes both live, so a run killed mid-flight still has
	// them), and the terminal result after agy exits. Leave it nil for tests that
	// only care about process lifecycle (cancel, timeout, liveness) and not about
	// what the job produced.
	Agy *FakeAgy
	// Out is the fixed content written to <dir>/out when AgyPath is empty.
	Out string
	// Exit is the fixed exit code recorded when AgyPath is empty.
	Exit int
	// CachePath, when set, makes the script write CacheJSON to that path
	// after exit_code, mimicking the real ordering: the supervisor writes the
	// exit sentinel at process exit, and agy's cache daemon flushes the
	// conversation cache around or after that moment.
	CachePath string
	// CacheJSON is the cache payload to write; requires CachePath.
	CacheJSON string
	// CacheDelay, when positive, writes the cache from a backgrounded subshell
	// that sleeps this long first, so the cache lands only after the script
	// (and its exit_code) is gone. This reproduces the cache-daemon lag that
	// the manager's capture retry exists for. Requires CachePath.
	CacheDelay time.Duration
}

// WriteFakeSupervisor writes an executable shell script that mimics the
// supervisor (`agy-mcp run-job <jobdir>`) and returns its path. The script is
// created under t.TempDir(). Fixed out and cache payloads are written to
// sibling files and reproduced via cat so arbitrary byte content survives
// shell quoting. exit_code is written before the cache payload, matching the
// real ordering the manager must tolerate: job completion (the sentinel) can
// be observable before the conversation cache has been flushed.
func WriteFakeSupervisor(t *testing.T, cfg FakeSupervisor) string {
	t.Helper()
	if cfg.AgyPath != "" && (cfg.Out != "" || cfg.Exit != 0) {
		t.Fatal("FakeSupervisor: AgyPath is mutually exclusive with Out/Exit")
	}
	if cfg.CacheJSON != "" && cfg.CachePath == "" {
		t.Fatal("FakeSupervisor: CacheJSON requires CachePath")
	}
	if cfg.CacheDelay > 0 && cfg.CachePath == "" {
		t.Fatal("FakeSupervisor: CacheDelay requires CachePath")
	}
	if cfg.Agy != nil && cfg.AgyPath == "" {
		t.Fatal("FakeSupervisor: Agy requires AgyPath")
	}
	dir := t.TempDir()
	// On Linux the basename doubles as the comm value; it must stay under the
	// kernel's 15-char comm limit for the liveness comm fallback to match.
	path := filepath.Join(dir, "fake-sup")

	var sb strings.Builder
	sb.WriteString("#!/usr/bin/env bash\n")
	if runtime.GOOS == "linux" {
		// Linux liveness matches /proc/<pid>/comm against the supervisor basename;
		// set it so the fake looks like the real supervisor. macOS pins identity
		// by start time instead, so the fake needs no comm trick there.
		sb.WriteString("printf '%s' \"${0##*/}\" > /proc/$$/comm\n")
	}
	sb.WriteString("dir=\"$2\"\n")
	// copyPayload stages content in a sibling file and emits the cat that puts it
	// into the job dir, so arbitrary bytes survive shell quoting.
	copyPayload := func(name, content, dest string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write fake supervisor %s payload: %v", name, err)
		}
		fmt.Fprintf(&sb, "cat %q > \"$dir/%s\"\n", p, dest)
	}

	// The marker the real supervisor writes before it execs agy (see
	// supervisor.markStreamJSON), so a staged job dir has the shape production
	// produces. Without it the manager's stream-json check would fall back to the
	// persisted args on every fake job, and the marker branch it exists to
	// exercise would never be reached through a spawned supervisor.
	//
	// A fake agy's own progress payload is written after this and replaces it,
	// exactly as consumeStream replaces the marker in production.
	fmt.Fprintf(&sb, "printf '%%s' %q > \"$dir/%s\"\n", `{"updated_at":"2026-01-01T00:00:00Z"}`, jobstore.ProgressFile)

	switch {
	case cfg.AgyPath != "":
		if cfg.Agy != nil {
			// Written before agy runs, mirroring the real supervisor's live writes:
			// a run killed mid-flight must still leave its streamed text and the
			// conversation id behind.
			copyPayload("stream-out", cfg.Agy.Stdout, jobstore.OutFile)
			copyPayload("stream-progress.json", cfg.Agy.ProgressJSON(t), jobstore.ProgressFile)
		}
		fmt.Fprintf(&sb, "%q -p x > /dev/null 2> \"$dir/%s\"\ncode=$?\n", cfg.AgyPath, jobstore.ErrFile)
		if cfg.Agy != nil && !cfg.Agy.OmitResult {
			// After agy exits and before the sentinel, exactly like the real
			// supervisor, so a sentinel visible to the manager implies a complete
			// result file.
			copyPayload("stream-result.json", cfg.Agy.ResultJSON(t), jobstore.ResultFile)
		}
	default:
		copyPayload("out-payload", cfg.Out, jobstore.OutFile)
		fmt.Fprintf(&sb, "code=%d\n", cfg.Exit)
	}
	fmt.Fprintf(&sb, "printf '%%s' \"$code\" > \"$dir/%s\"\n", jobstore.ExitCodeFile)
	if cfg.CachePath != "" {
		cachePayload := filepath.Join(dir, "cache-payload.json")
		if err := os.WriteFile(cachePayload, []byte(cfg.CacheJSON), 0o644); err != nil {
			t.Fatalf("write fake supervisor cache payload: %v", err)
		}
		if cfg.CacheDelay > 0 {
			// Backgrounded so the script (the fake supervisor process) exits
			// first and the cache lands later, like agy's real cache daemon.
			fmt.Fprintf(&sb, "( sleep %.3f; cat %q > %q ) &\n",
				cfg.CacheDelay.Seconds(), cachePayload, cfg.CachePath)
		} else {
			fmt.Fprintf(&sb, "cat %q > %q\n", cachePayload, cfg.CachePath)
		}
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0o755); err != nil {
		t.Fatalf("write fake supervisor: %v", err)
	}
	return path
}
