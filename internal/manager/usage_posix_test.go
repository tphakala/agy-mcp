//go:build linux || darwin

package manager

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

func geminiUsage(remaining float64) []testutil.FakeQuotaGroup {
	return []testutil.FakeQuotaGroup{{
		Name: "Gemini Models",
		Buckets: []testutil.FakeQuotaBucket{
			{ID: "5h", Name: "5h limit", Window: "5h", RemainingFraction: remaining, ResetTime: "2099-01-01T00:00:00Z"},
		},
	}}
}

func TestUsageProbeOverFakeAgy(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Usage: geminiUsage(0.42)})
	m := newManager(t, managerOpts{agyPath: agy})
	snap, err := m.Usage(t.Context(), time.Minute)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(snap.Groups) != 1 || snap.Groups[0].Name != "Gemini Models" || snap.Groups[0].Buckets[0].RemainingFraction != 0.42 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestUsageProbeIncludesStderrOnError(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Exit: 1, Stderr: "agy: not logged in"})
	m := newManager(t, managerOpts{agyPath: agy})
	_, err := m.Usage(t.Context(), time.Minute)
	if err == nil || !strings.HasPrefix(err.Error(), "agy /usage:") || !strings.Contains(err.Error(), "agy: not logged in") {
		t.Fatalf("error = %v, want one starting with %q carrying the stderr", err, "agy /usage:")
	}
}

// The probe must run in its own session: agy opens /dev/tty in -p mode and stops
// on SIGTTOU in a background process group (see proc.ConfigureSession). The fake
// records its session id and its parent's; Setsid makes them differ.
func TestUsageProbeRunsInOwnSession(t *testing.T) {
	dir := t.TempDir()
	sidFile := filepath.Join(dir, "sid")
	script := filepath.Join(dir, "agy")
	body := `#!/bin/sh
if [ "$1" = "--version" ]; then echo ` + "1.9.9" + `; exit 0; fi
echo "$(ps -o sid= -p $$ | tr -d ' ') $(ps -o sid= -p $PPID | tr -d ' ')" > ` + sidFile + `
echo '{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}'
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, managerOpts{agyPath: script})
	if _, err := m.Usage(t.Context(), time.Minute); err != nil {
		t.Fatalf("Usage: %v", err)
	}
	raw, err := os.ReadFile(sidFile)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 || fields[0] == fields[1] {
		t.Fatalf("child and parent sessions = %q, want them to differ", raw)
	}
}

func quotaJobManager(t *testing.T) *Manager {
	t.Helper()
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "done", Sleep: 30 * time.Second})
	m := newManager(t, managerOpts{
		agyPath:        "/usr/bin/agy",
		supervisorExe:  testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{AgyPath: agy}),
		defaultTimeout: time.Minute,
		maxConcurrency: 4,
		withCacheFile:  true,
	})
	m.cfg.QuotaLow, m.cfg.QuotaCritical = 0.25, 0.05
	return m
}

func setSnapshot(m *Manager, remaining float64) {
	m.quota.mu.Lock()
	defer m.quota.mu.Unlock()
	m.quota.snap = quotaFixture(m.now(), remaining, m.now().Add(time.Hour))
	m.quota.have = true
}

const quotaTestModel = "gemini-3.8-flash-high"

func TestStartJobOptionalReplaysBoundJob(t *testing.T) {
	m := quotaJobManager(t)
	req := StartRequest{Prompt: "review", Cwd: t.TempDir(), Model: quotaTestModel, IdempotencyKey: "k", Priority: PriorityOptional}
	setSnapshot(m, 0.9)
	first, err := m.StartJob(req)
	if err != nil {
		t.Fatalf("first StartJob: %v", err)
	}
	killJob(t, m, first.ID)
	setSnapshot(m, 0.02)
	second, err := m.StartJob(req)
	if err != nil {
		t.Fatalf("replay while low: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned job %s, want %s", second.ID, first.ID)
	}
}

func TestStartJobOptionalRefusedLeavesNoTrace(t *testing.T) {
	m := quotaJobManager(t)
	cwd := t.TempDir()
	setSnapshot(m, 0.02)
	req := StartRequest{Prompt: "review", Cwd: cwd, Model: quotaTestModel, IdempotencyKey: "k", Priority: PriorityOptional}
	_, err := m.StartJob(req)
	if _, ok := errors.AsType[*QuotaRefusalError](err); !ok {
		t.Fatalf("StartJob error = %v, want *QuotaRefusalError", err)
	}
	ids, err := m.store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("jobs after a refusal = %v, want none", ids)
	}
	// The claim was released and nothing was bound to the key: the same key now
	// starts a fresh job once the run is no longer optional.
	req.Priority = PriorityNormal
	job, err := m.StartJob(req)
	if err != nil {
		t.Fatalf("normal StartJob with the refused key: %v", err)
	}
	killJob(t, m, job.ID)
}

func TestStartJobNormalIgnoresQuota(t *testing.T) {
	m := quotaJobManager(t)
	setSnapshot(m, 0)
	job, err := m.StartJob(StartRequest{Prompt: "review", Cwd: t.TempDir(), Model: quotaTestModel})
	if err != nil {
		t.Fatalf("normal run on exhausted quota: %v", err)
	}
	killJob(t, m, job.ID)
}
