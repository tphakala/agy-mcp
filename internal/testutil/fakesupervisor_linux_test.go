package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runSupervisor executes the fake supervisor the way the manager does
// (`<exe> run-job <jobdir>`) and returns the job dir.
func runSupervisor(t *testing.T, sup string) string {
	t.Helper()
	dir := t.TempDir()
	if err := exec.Command(sup, "run-job", dir).Run(); err != nil {
		t.Fatalf("run fake supervisor: %v", err)
	}
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestFakeSupervisorFixedOutAndExit(t *testing.T) {
	sup := WriteFakeSupervisor(t, FakeSupervisor{Out: "done", Exit: 3})
	dir := runSupervisor(t, sup)
	if got := readFile(t, filepath.Join(dir, "out")); got != "done" {
		t.Errorf("out = %q, want %q", got, "done")
	}
	if got := readFile(t, filepath.Join(dir, "exit_code")); got != "3" {
		t.Errorf("exit_code = %q, want %q", got, "3")
	}
}

func TestFakeSupervisorRunsAgy(t *testing.T) {
	agy := WriteFakeAgy(t, FakeAgy{Stdout: "hello", Stderr: "warn", Exit: 2})
	sup := WriteFakeSupervisor(t, FakeSupervisor{AgyPath: agy})
	dir := runSupervisor(t, sup)
	if got := readFile(t, filepath.Join(dir, "out")); got != "hello" {
		t.Errorf("out = %q, want %q", got, "hello")
	}
	if got := readFile(t, filepath.Join(dir, "err")); got != "warn" {
		t.Errorf("err = %q, want %q", got, "warn")
	}
	if got := readFile(t, filepath.Join(dir, "exit_code")); got != "2" {
		t.Errorf("exit_code = %q, want %q", got, "2")
	}
}

func TestFakeSupervisorWritesCache(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "last_conversations.json")
	const payload = `{"/some/cwd":"uuid-1234"}`
	sup := WriteFakeSupervisor(t, FakeSupervisor{Out: "done", CachePath: cachePath, CacheJSON: payload})
	dir := runSupervisor(t, sup)
	if got := readFile(t, cachePath); got != payload {
		t.Errorf("cache = %q, want %q", got, payload)
	}
	if got := readFile(t, filepath.Join(dir, "exit_code")); got != "0" {
		t.Errorf("exit_code = %q, want %q", got, "0")
	}
}
