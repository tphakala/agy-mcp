package manager

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
	"github.com/tphakala/agy-mcp/v2/internal/config"
)

// versionManager builds a manager whose agy path resolves without a real binary
// and whose version probe returns raw (or err).
func versionManager(t *testing.T, raw string, err error) (*Manager, *atomic.Int32) {
	t.Helper()
	m := New(config.Config{AgyPath: "/usr/bin/agy", StateDir: t.TempDir(), MaxConcurrency: 4})
	var calls atomic.Int32
	m.readAgyVersion = func(context.Context, string) (string, error) {
		calls.Add(1)
		return raw, err
	}
	return m, &calls
}

func TestAgyBinaryCheckedAcceptsSupportedVersion(t *testing.T) {
	m, calls := versionManager(t, agyver.Required.String()+"\n", nil)
	got, err := m.agyBinaryChecked(t.Context())
	if err != nil {
		t.Fatalf("agyBinaryChecked: %v", err)
	}
	if got != "/usr/bin/agy" {
		t.Fatalf("path = %q", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("probe ran %d times, want 1", n)
	}
}

// A verified binary is probed once per process: the whole point of caching is
// that a tool call does not pay a process spawn every time.
func TestAgyBinaryCheckedCachesSuccess(t *testing.T) {
	m, calls := versionManager(t, "1.2.0", nil)
	for range 5 {
		if _, err := m.agyBinaryChecked(t.Context()); err != nil {
			t.Fatalf("agyBinaryChecked: %v", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("probe ran %d times, want 1 (result must be cached)", n)
	}
}

func TestAgyBinaryCheckedRefusesOldVersion(t *testing.T) {
	m, _ := versionManager(t, "1.1.7", nil)
	_, err := m.agyBinaryChecked(t.Context())
	if err == nil {
		t.Fatal("an agy older than the floor must be refused")
	}
	// The message has to name both what is needed and what was found, or the
	// reader cannot tell which half of the mismatch to fix.
	for _, want := range []string{agyver.Required.String(), "1.1.7", "/usr/bin/agy", "stream-json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A refusal is deliberately not cached, so upgrading agy is picked up without
// restarting the server, matching the deferred PATH lookup's promise.
func TestAgyBinaryCheckedRetriesAfterRefusal(t *testing.T) {
	m := New(config.Config{AgyPath: "/usr/bin/agy", StateDir: t.TempDir(), MaxConcurrency: 4})
	var version atomic.Value
	version.Store("1.1.7")
	var calls atomic.Int32
	m.readAgyVersion = func(context.Context, string) (string, error) {
		calls.Add(1)
		v, _ := version.Load().(string)
		return v, nil
	}

	if _, err := m.agyBinaryChecked(t.Context()); err == nil {
		t.Fatal("precondition: the old version must be refused")
	}
	version.Store("1.1.8") // the user upgrades agy mid-session
	if _, err := m.agyBinaryChecked(t.Context()); err != nil {
		t.Fatalf("an upgraded agy must be accepted without a restart: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("probe ran %d times, want 2 (a failure must not be cached)", n)
	}
}

func TestAgyBinaryCheckedReportsUnparseableOutput(t *testing.T) {
	m, _ := versionManager(t, "who knows", nil)
	_, err := m.agyBinaryChecked(t.Context())
	if err == nil {
		t.Fatal("output with no version number must be an error")
	}
	if !strings.Contains(err.Error(), "who knows") {
		t.Errorf("error %q should quote what agy actually printed", err)
	}
}

func TestAgyBinaryCheckedReportsProbeFailure(t *testing.T) {
	m, _ := versionManager(t, "", errors.New("permission denied"))
	_, err := m.agyBinaryChecked(t.Context())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the probe failure surfaced", err)
	}
}

// A missing agy is reported as a lookup failure, not a version failure, and
// never runs the probe: there is nothing to probe.
func TestAgyBinaryCheckedWithoutBinary(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4})
	var calls atomic.Int32
	m.readAgyVersion = func(context.Context, string) (string, error) {
		calls.Add(1)
		return agyver.Required.String(), nil
	}
	t.Setenv("PATH", t.TempDir()) // no agy anywhere on it
	if _, err := m.agyBinaryChecked(t.Context()); err == nil {
		t.Fatal("a missing agy must be an error")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("probe ran %d times for a binary that could not be resolved", n)
	}
}
