package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/config"
)

// fakeSupervisor writes a script that mimics `agy-mcp run-job <dir>`:
// it writes out="done" and exit_code=0 for the given job dir.
func fakeSupervisor(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-sup")
	script := "#!/usr/bin/env bash\n" +
		"dir=\"$2\"\n" +
		"printf 'done' > \"$dir/out\"\n" +
		"printf '0' > \"$dir/exit_code\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStartJobPersistsMetaAndSpawns(t *testing.T) {
	state := t.TempDir()
	c := config.Config{
		AgyPath:        "/usr/bin/agy",
		SupervisorExe:  fakeSupervisor(t),
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
