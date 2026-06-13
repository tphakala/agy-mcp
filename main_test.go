package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func jsonMarshalForTest(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// builtBin holds the path buildBinary compiled to, so TestMain can remove it
// after the run. It is written inside the buildBinary once-func (which completes
// before any test using the binary returns) and read in TestMain only after
// m.Run, so there is no race.
var builtBin string

// buildBinary compiles the agy-mcp binary once for the whole package test run and
// returns its path; the two run-job tests share it instead of each rebuilding.
// Lazy (built on first use) so a focused run of the pure tests below pays nothing.
// CreateTemp gives a unique path for this one shared binary; t.TempDir/t.ArtifactDir
// are per-test and unavailable here (this is package-level, with no *testing.T),
// so TestMain owns the cleanup instead.
var buildBinary = sync.OnceValues(func() (string, error) {
	f, err := os.CreateTemp("", "agy-mcp-*")
	if err != nil {
		return "", err
	}
	bin := f.Name()
	_ = f.Close() // go build -o overwrites the placeholder file with the binary
	builtBin = bin
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("build agy-mcp: %w\n%s", err, out)
	}
	return bin, nil
})

func TestMain(m *testing.M) {
	code := m.Run()
	if builtBin != "" {
		_ = os.Remove(builtBin)
	}
	os.Exit(code)
}

// Build the binary once and use it as its own supervisor against a fake agy.
func TestRunJobSubcommandEndToEnd(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "E2E OK", Exit: 0})

	jobDir := t.TempDir()
	meta := jobstore.Meta{ID: filepath.Base(jobDir), AgyPath: agy, Args: []string{"-p", "x"}}
	b, _ := jsonMarshalForTest(meta)
	_ = os.WriteFile(jobstore.MetaPath(jobDir), b, 0o644)

	cmd := exec.Command(bin, "run-job", jobDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run-job: %v\n%s", err, out)
	}
	out, _ := os.ReadFile(jobstore.OutPath(jobDir))
	if strings.TrimSpace(string(out)) != "E2E OK" {
		t.Fatalf("out = %q", out)
	}
}

// Cancel end to end: SIGTERM the supervisor subprocess and confirm it forwards
// the signal to agy and writes the SIGTERM sentinel, which Status maps to
// "cancelled".
func TestRunJobCancelViaSignal(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}
	// A fake agy that sleeps far longer than the test; with no timeout in meta,
	// only an external cancel signal can stop it.
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "x", SleepSecs: 60})
	jobDir := t.TempDir()
	meta := jobstore.Meta{ID: filepath.Base(jobDir), AgyPath: agy, Args: []string{"-p", "x"}}
	b, _ := jsonMarshalForTest(meta)
	_ = os.WriteFile(jobstore.MetaPath(jobDir), b, 0o644)

	cmd := exec.Command(bin, "run-job", jobDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// On any fatal path below (before the happy-path Wait), tear the supervisor
	// down so neither it nor its 60s fake agy child leaks. SIGTERM lets the
	// supervisor forward the signal to agy and reap it, unlike a bare Kill.
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	})

	// Wait for the supervisor to create the job's out/err files, which it does
	// immediately before installing its SIGTERM handler and starting agy. Polling
	// for that instead of sleeping a fixed interval removes the race where, under
	// CI load, the supervisor process has not started yet and the SIGTERM lands on
	// a handlerless process (which would die without writing the sentinel, leaking
	// the 60s agy and burning the poll below). The window between file creation and
	// signal.Notify is a few non-blocking calls, so it is microseconds and, unlike
	// process spawn, does not stretch under load.
	startDeadline := time.Now().Add(5 * time.Second)
	for {
		_, oerr := os.Stat(jobstore.OutPath(jobDir))
		_, eerr := os.Stat(jobstore.ErrPath(jobDir))
		if oerr == nil && eerr == nil {
			break
		}
		if time.Now().After(startDeadline) {
			t.Fatal("supervisor did not create out/err; cannot safely signal it")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	reaped = true

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(jobstore.ExitCodePath(jobDir)); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	code, _ := os.ReadFile(jobstore.ExitCodePath(jobDir))
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
