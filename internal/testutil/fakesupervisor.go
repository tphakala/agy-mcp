package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
)

// FakeSupervisor configures a stand-in supervisor script for tests.
//
// The generated script mimics `agy-mcp run-job <jobdir>`: it takes the job
// directory as $2, writes the job's out (and err/exit_code) files there, and
// sets its comm to the script basename so the liveness comm fallback sees the
// same value the real supervisor would report.
type FakeSupervisor struct {
	// AgyPath, when set, makes the script run that (fake) agy binary with
	// `-p x`, streaming stdout to <dir>/out and stderr to <dir>/err and
	// recording agy's real exit code. Mutually exclusive with Out/Exit.
	AgyPath string
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
	dir := t.TempDir()
	// The basename doubles as the comm value; it must stay under the kernel's
	// 15-char comm limit for the liveness comm fallback to match.
	path := filepath.Join(dir, "fake-sup")

	var sb strings.Builder
	sb.WriteString("#!/usr/bin/env bash\n")
	sb.WriteString("printf '%s' \"${0##*/}\" > /proc/$$/comm\n")
	sb.WriteString("dir=\"$2\"\n")
	if cfg.AgyPath != "" {
		fmt.Fprintf(&sb, "%q -p x > \"$dir/%s\" 2> \"$dir/%s\"\ncode=$?\n", cfg.AgyPath, jobstore.OutFile, jobstore.ErrFile)
	} else {
		outPayload := filepath.Join(dir, "out-payload")
		if err := os.WriteFile(outPayload, []byte(cfg.Out), 0o644); err != nil {
			t.Fatalf("write fake supervisor out payload: %v", err)
		}
		fmt.Fprintf(&sb, "cat %q > \"$dir/%s\"\ncode=%d\n", outPayload, jobstore.OutFile, cfg.Exit)
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
