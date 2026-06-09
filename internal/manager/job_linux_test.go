package manager

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func TestStartJobPersistsMetaAndSpawns(t *testing.T) {
	state := t.TempDir()
	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{Out: "done"}),
		StateDir:       state,
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	}
	m := New(c)

	job, err := m.StartJob(StartRequest{Prompt: "review main.go", Model: "Gemini 3.1 Pro (High)"})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if job.ID == "" {
		t.Fatal("empty job id")
	}

	// meta.json must exist and contain the prompt and agy args.
	meta, err := m.store.Load(job.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if meta.Prompt != "review main.go" {
		t.Errorf("prompt = %q", meta.Prompt)
	}
	if !hasArg(meta.Args, "--model", "Gemini 3.1 Pro (High)") {
		t.Errorf("args missing model: %v", meta.Args)
	}
	if !contains(meta.Args, "-p") || !contains(meta.Args, "--dangerously-skip-permissions") {
		t.Errorf("args missing required flags: %v", meta.Args)
	}

	// The fake supervisor writes out/exit_code; wait briefly for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := m.store.ExitCode(job.ID); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := m.store.ExitCode(job.ID); !ok {
		t.Fatal("supervisor did not write exit_code")
	}
}

func TestStartJobCleansUpDirOnSpawnFailure(t *testing.T) {
	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  filepath.Join(t.TempDir(), "nonexistent-supervisor"),
		StateDir:       t.TempDir(),
		DefaultTimeout: time.Minute,
		MaxConcurrency: 4,
	}
	m := New(c)
	cwd := t.TempDir()

	_, err := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd})
	if err == nil || !strings.Contains(err.Error(), "spawn supervisor") {
		t.Fatalf("StartJob error = %v, want a spawn-supervisor failure", err)
	}

	// The job directory created before the failed spawn must be removed, not left
	// orphaned for GarbageCollect to reap later.
	ids, lerr := m.store.List()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(ids) != 0 {
		t.Fatalf("orphaned job dir left on disk after spawn failure: %v", ids)
	}

	// The gate slot/key must also be released: a second same-cwd run fails at spawn
	// again, rather than being refused by the gate ("conflicting job").
	_, err2 := m.StartJob(StartRequest{Prompt: "x", Cwd: cwd})
	if err2 == nil || !strings.Contains(err2.Error(), "spawn supervisor") {
		t.Fatalf("second run error = %v, want spawn-supervisor (gate slot leaked?)", err2)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func hasArg(ss []string, flag, val string) bool {
	for i := 0; i+1 < len(ss); i++ {
		if ss[i] == flag && ss[i+1] == val {
			return true
		}
	}
	return false
}
