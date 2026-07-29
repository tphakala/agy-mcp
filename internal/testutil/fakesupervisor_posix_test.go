//go:build linux || darwin

package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

// runSupervisor executes the fake supervisor the way the manager does
// (`<exe> run-job <jobdir>`) and returns the job dir. It runs under a hard timeout
// so a hung script fails the test with diagnostics instead of stalling the suite.
func runSupervisor(t *testing.T, sup string) string {
	t.Helper()
	dir := t.TempDir()
	if res := runScript(t, 10*time.Second, sup, "run-job", dir); res.ExitCode != 0 {
		t.Fatalf("fake supervisor exited %d; stderr: %q", res.ExitCode, res.Stderr)
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

// With Agy set, the fake writes the same job-dir files the real supervisor
// derives from the stream: the decoded response in out, the conversation in
// progress.json, and the terminal payload in result.json.
func TestFakeSupervisorRunsAgyAndWritesStreamFiles(t *testing.T) {
	cfg := FakeAgy{Stdout: "hello", Stderr: "warn", Exit: 2}
	agy := WriteFakeAgy(t, cfg)
	sup := WriteFakeSupervisor(t, FakeSupervisor{AgyPath: agy, Agy: &cfg})
	dir := runSupervisor(t, sup)
	if got := readFile(t, filepath.Join(dir, "out")); got != "hello" {
		t.Errorf("out = %q, want the decoded response %q", got, "hello")
	}
	if got := readFile(t, filepath.Join(dir, "err")); got != "warn" {
		t.Errorf("err = %q, want %q", got, "warn")
	}
	if got := readFile(t, filepath.Join(dir, "exit_code")); got != "2" {
		t.Errorf("exit_code = %q, want %q", got, "2")
	}
	var res streamjson.Result
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "result.json"))), &res); err != nil {
		t.Fatalf("result.json: %v", err)
	}
	if res.Response != "hello" || res.ConversationID != cfg.ConvID() {
		t.Errorf("result.json = %+v, want the response and conversation of the run", res)
	}
	var prog jobstore.Progress
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "progress.json"))), &prog); err != nil {
		t.Fatalf("progress.json: %v", err)
	}
	if prog.ConversationID != cfg.ConvID() {
		t.Errorf("progress conversation = %q, want %q", prog.ConversationID, cfg.ConvID())
	}
}

// Without Agy the fake only runs the binary; agy's stdout is discarded because
// it is an event stream the real supervisor decodes rather than copies.
func TestFakeSupervisorRunsAgyWithoutStreamFiles(t *testing.T) {
	agy := WriteFakeAgy(t, FakeAgy{Stdout: "hello", Stderr: "warn", Exit: 2})
	sup := WriteFakeSupervisor(t, FakeSupervisor{AgyPath: agy})
	dir := runSupervisor(t, sup)
	if _, err := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Errorf("out should not exist without an Agy config, stat err = %v", err)
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
