package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func jsonMarshalForTest(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// Build the binary once and use it as its own supervisor against a fake agy.
func TestRunJobSubcommandEndToEnd(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agy-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "E2E OK", Exit: 0})

	jobDir := t.TempDir()
	meta := jobstore.Meta{ID: filepath.Base(jobDir), AgyPath: agy, Args: []string{"-p", "x"}}
	b, _ := jsonMarshalForTest(meta)
	_ = os.WriteFile(filepath.Join(jobDir, "meta.json"), b, 0o644)

	cmd := exec.Command(bin, "run-job", jobDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run-job: %v\n%s", err, out)
	}
	out, _ := os.ReadFile(filepath.Join(jobDir, "out"))
	if strings.TrimSpace(string(out)) != "E2E OK" {
		t.Fatalf("out = %q", out)
	}
}

// Cancel end to end: SIGTERM the supervisor subprocess and confirm it forwards
// the signal to agy and writes the SIGTERM sentinel, which Status maps to
// "cancelled".
func TestRunJobCancelViaSignal(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agy-mcp")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// A fake agy that sleeps far longer than the test; with no timeout in meta,
	// only an external cancel signal can stop it.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", SleepSecs: 60})
	jobDir := t.TempDir()
	meta := jobstore.Meta{ID: filepath.Base(jobDir), AgyPath: agy, Args: []string{"-p", "x"}}
	b, _ := jsonMarshalForTest(meta)
	_ = os.WriteFile(filepath.Join(jobDir, "meta.json"), b, 0o644)

	cmd := exec.Command(bin, "run-job", jobDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Give the supervisor time to start agy and install its SIGTERM handler.
	time.Sleep(time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(jobDir, "exit_code")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	code, _ := os.ReadFile(filepath.Join(jobDir, "exit_code"))
	if strings.TrimSpace(string(code)) != strconv.Itoa(jobstore.ExitSIGTERM) {
		t.Fatalf("exit_code = %q, want %d (SIGTERM cancel)", code, jobstore.ExitSIGTERM)
	}
}

func TestCheckLoopbackAddr(t *testing.T) {
	// Literal IPs and localhost (resolved via the real /etc/hosts) are loopback;
	// hostname resolution corner cases are covered hermetically below.
	for _, addr := range []string{"127.0.0.1:8765", "localhost:8765", "[::1]:8765", "127.0.0.1:0", "[::1]"} {
		if err := checkLoopbackAddr(addr); err != nil {
			t.Errorf("checkLoopbackAddr(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range []string{":8765", "0.0.0.0:8765", "192.168.1.10:8765"} {
		if err := checkLoopbackAddr(addr); err == nil {
			t.Errorf("checkLoopbackAddr(%q) = nil, want error", addr)
		}
	}
}

func TestCheckLoopbackAddrResolvesHostnames(t *testing.T) {
	loopback := func(string) ([]string, error) { return []string{"127.0.0.1", "::1"}, nil }
	remapped := func(string) ([]string, error) { return []string{"127.0.0.1", "10.0.0.5"}, nil }
	failing := func(string) ([]string, error) { return nil, errors.New("no such host") }
	empty := func(string) ([]string, error) { return nil, nil }

	if err := checkLoopbackAddrResolved("localhost:8765", loopback); err != nil {
		t.Errorf("localhost resolving to loopback should pass, got %v", err)
	}
	// A hosts-file remap of localhost to a routable IP must be rejected even though
	// one resolved address is still loopback: any non-loopback address exposes it.
	if err := checkLoopbackAddrResolved("localhost:8765", remapped); err == nil {
		t.Error("localhost remapped to a non-loopback address must be rejected")
	}
	if err := checkLoopbackAddrResolved("localhost:8765", failing); err == nil {
		t.Error("a resolve failure must be rejected, not silently allowed")
	}
	if err := checkLoopbackAddrResolved("localhost:8765", empty); err == nil {
		t.Error("a host resolving to no addresses must be rejected")
	}
	// A literal IP needs no resolution and must not call the resolver.
	called := false
	spy := func(string) ([]string, error) { called = true; return nil, nil }
	if err := checkLoopbackAddrResolved("127.0.0.1:8765", spy); err != nil {
		t.Errorf("literal loopback IP should pass, got %v", err)
	}
	if called {
		t.Error("a literal IP must not trigger a hostname lookup")
	}
}
