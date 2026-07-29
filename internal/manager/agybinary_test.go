package manager

import (
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

// noAgyOnPath empties PATH so exec.LookPath("agy") cannot succeed. It models the
// state config.Resolve now tolerates: a server that started with no agy
// installed, leaving the lookup to the first call that actually execs it.
func noAgyOnPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// TestListModelsWithoutAgyReportsClearly: the startup error moved to the call
// site, so it must arrive intact. Execing an empty AgyPath would surface as
// "exec: no command", which says nothing about what the operator should fix.
func TestListModelsWithoutAgyReportsClearly(t *testing.T) {
	noAgyOnPath(t)
	m := newManager(t, managerOpts{})

	_, err := m.ListModels(t.Context())
	if err == nil {
		t.Fatal("ListModels must fail when agy cannot be resolved")
	}
	if !strings.Contains(err.Error(), "agy not found on PATH") {
		t.Errorf("error should name the missing binary, got: %v", err)
	}
}

// TestStartJobWithoutAgyReportsClearlyAndHoldsNoGate covers the same message on
// the job path, plus where the lookup sits: before admission. Resolving after
// admit would let a missing agy burn a concurrency slot and hold the cwd key,
// blocking every later same-cwd run for the life of the process.
func TestStartJobWithoutAgyReportsClearlyAndHoldsNoGate(t *testing.T) {
	if !proc.Supported {
		t.Skip("StartJob refuses on platforms without job supervision before it ever looks for agy")
	}
	noAgyOnPath(t)
	m := newManager(t, managerOpts{})

	_, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("StartJob must fail when agy cannot be resolved")
	}
	if !strings.Contains(err.Error(), "agy not found on PATH") {
		t.Errorf("error should name the missing binary, got: %v", err)
	}
	if m.gate.inFlight != 0 {
		t.Errorf("in-flight = %d, want 0: a refused start must not consume a slot", m.gate.inFlight)
	}
	if len(m.gate.keys) != 0 {
		t.Errorf("gate holds %v, want no keys: a refused start must not block later same-cwd runs", m.gate.keys)
	}
}
