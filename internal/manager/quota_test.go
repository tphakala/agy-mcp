package manager

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/config"
)

var testThresholds = quotaThresholds{Low: 0.25, Critical: 0.05}

// measuredUsageEnvelope is the shape agy 1.2.16 prints for
// `agy --output-format json -p /usage` (MEASURED), trimmed to two groups.
const measuredUsageEnvelope = `{"conversation_id":"","status":"SUCCESS","response":"x","duration_seconds":0,"num_turns":0,
"usage":{"input_tokens":0},"command":{"name":"usage","data":{"description":"d","groups":[
{"name":"Gemini Models","description":"Gemini family","buckets":[
 {"id":"5h","name":"5h limit","window":"5h","remaining_fraction":0.03,"reset_time":"2026-10-04T11:32:45Z"},
 {"id":"weekly","name":"Weekly limit","description":"w","window":"weekly","remaining_fraction":0.68,"reset_time":"2026-10-07T08:00:00Z"}]},
{"name":"Claude and GPT Models","description":"Others","buckets":[
 {"id":"5h","name":"5h limit","window":"5h","remaining_fraction":1,"reset_time":"2026-10-04T12:00:00Z"},
 {"id":"weekly","name":"Weekly limit","window":"weekly","remaining_fraction":1,"reset_time":"2026-10-07T08:00:00Z"}]}]}}}`

func TestDecodeUsageEnvelope(t *testing.T) {
	t.Run("measured envelope", func(t *testing.T) {
		groups, err := decodeUsageEnvelope([]byte(measuredUsageEnvelope))
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 2 || len(groups[0].Buckets) != 2 || len(groups[1].Buckets) != 2 {
			t.Fatalf("groups = %+v, want two groups of two buckets", groups)
		}
		b := groups[0].Buckets[0]
		if b.ID != "5h" || b.RemainingFraction != 0.03 || b.Window != "5h" ||
			!b.ResetTime.Equal(time.Date(2026, 10, 4, 11, 32, 45, 0, time.UTC)) {
			t.Fatalf("first bucket = %+v", b)
		}
	})
	tests := []struct {
		name    string
		raw     string
		wantErr string // substring; empty means success
		groups  int
	}{
		{"status error", `{"status":"ERROR","command":{"name":"usage","data":{"groups":[]}}}`, "agy usage", 0},
		{"other command", `{"status":"SUCCESS","command":{"name":"models","data":{"groups":[]}}}`, "agy usage", 0},
		{"missing groups", `{"status":"SUCCESS","command":{"name":"usage","data":{}}}`, "agy usage", 0},
		{"null groups", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":null}}}`, "agy usage", 0},
		{"not json", `nope`, "agy usage", 0},
		{"empty groups is a valid reading", `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}`, "", 0},
		{"bucket without remaining is skipped",
			`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"g","buckets":[{"id":"a"},{"id":"b","remaining_fraction":0.5}]}]}}}`, "", 1},
		{"no bucket carries remaining",
			`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"g","buckets":[{"id":"a"}]}]}}}`, "remaining_fraction", 0},
		{"bad reset time",
			`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"g","buckets":[{"id":"a","remaining_fraction":1,"reset_time":"tomorrow"}]}]}}}`, "reset_time", 0},
		{"bucket without reset time is tolerated",
			`{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"g","buckets":[{"id":"a","remaining_fraction":1}]}]}}}`, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups, err := decodeUsageEnvelope([]byte(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(groups) != tt.groups {
				t.Fatalf("groups = %d, want %d", len(groups), tt.groups)
			}
		})
	}
}

// The measured --disable-slash-commands reply: a model turn, no command field.
func TestDecodeUsageEnvelopeDetectsModelTurn(t *testing.T) {
	raw := `{"conversation_id":"abc-123","status":"SUCCESS","response":"hi","num_turns":1,"usage":{"input_tokens":21214}}`
	_, err := decodeUsageEnvelope([]byte(raw))
	if !errors.Is(err, errUsageModelTurn) {
		t.Fatalf("error = %v, want errUsageModelTurn", err)
	}
	// A SUCCESS envelope with no command and no turn is a shape error, not a latch.
	_, err = decodeUsageEnvelope([]byte(`{"status":"SUCCESS"}`))
	if err == nil || errors.Is(err, errUsageModelTurn) {
		t.Fatalf("error = %v, want a plain framing error", err)
	}
}

func TestUsageProbeArgsSpendNoQuota(t *testing.T) {
	got := usageProbeArgs()
	want := []string{"--output-format", "json", "-p", "/usage"}
	if !slices.Equal(got, want) {
		t.Fatalf("usageProbeArgs() = %q, want %q", got, want)
	}
	if slices.Contains(got, disableSlashCommandsFlag) {
		t.Fatal("the usage probe must never carry --disable-slash-commands")
	}
}

func TestQuotaLevelBoundaries(t *testing.T) {
	th := quotaThresholds{Low: 0.15, Critical: 0.05}
	tests := []struct {
		r    float64
		want QuotaLevel
	}{
		{1, QuotaOK}, {0.15, QuotaOK}, {0.1499, QuotaLow}, {0.05, QuotaLow},
		{0.0499, QuotaCritical}, {0.0001, QuotaCritical}, {0, QuotaExhausted}, {-0.1, QuotaExhausted},
	}
	for _, tt := range tests {
		// Through groupQuota so the clamp of a negative fraction is covered too.
		g := QuotaGroup{Buckets: []QuotaBucket{{RemainingFraction: tt.r}}}
		if got := groupQuota(g, time.Now(), th).Level; got != tt.want {
			t.Errorf("remaining %v: level = %s, want %s", tt.r, got, tt.want)
		}
	}
}

func TestQuotaLevelMonotonic(t *testing.T) {
	severity := map[QuotaLevel]int{QuotaOK: 0, QuotaLow: 1, QuotaCritical: 2, QuotaExhausted: 3}
	prop := func(a, b, lo, cr uint16) bool {
		r1, r2 := float64(a%1001)/1000, float64(b%1001)/1000
		if r1 > r2 {
			r1, r2 = r2, r1
		}
		low, crit := float64(lo%1001)/1000, float64(cr%1001)/1000
		if crit > low {
			low, crit = crit, low
		}
		th := quotaThresholds{Low: low, Critical: crit}
		return severity[levelOf(r1, th)] >= severity[levelOf(r2, th)]
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Fatal(err)
	}
}

func TestGroupQuotaBindingBucket(t *testing.T) {
	t5 := time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)
	tw := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	g := QuotaGroup{Name: "Gemini", Buckets: []QuotaBucket{
		{ID: "5h", RemainingFraction: 0.03, ResetTime: t5},
		{ID: "weekly", RemainingFraction: 0.68, ResetTime: tw},
	}}
	before := t5.Add(-time.Hour)
	gq := groupQuota(g, before, testThresholds)
	if gq.Binding.ID != "5h" || gq.Level != QuotaCritical || gq.Percent() != 3 || gq.Buckets[0].Refilled {
		t.Fatalf("before reset: %+v (percent %d)", gq, gq.Percent())
	}
	// Past the 5h reset that bucket counts as full, so the weekly one binds.
	gq = groupQuota(g, t5.Add(time.Minute), testThresholds)
	if gq.Binding.ID != "weekly" || gq.Level != QuotaOK || !gq.Buckets[0].Refilled || gq.Buckets[1].Refilled || gq.Percent() != 68 {
		t.Fatalf("after reset: %+v", gq)
	}

	// A tie binds the later reset.
	tie := QuotaGroup{Buckets: []QuotaBucket{
		{ID: "early", RemainingFraction: 0.1, ResetTime: t5},
		{ID: "late", RemainingFraction: 0.1, ResetTime: tw},
	}}
	if got := groupQuota(tie, before, testThresholds).Binding.ID; got != "late" {
		t.Fatalf("tie binding = %s, want late", got)
	}
	// Percent rounds down: 0.0356 reads 3, and 0.29 does not read 28 through float error.
	if p := (GroupQuota{Remaining: 0.0356}).Percent(); p != 3 {
		t.Fatalf("Percent(0.0356) = %d, want 3", p)
	}
	if p := (GroupQuota{Remaining: 0.29}).Percent(); p != 29 {
		t.Fatalf("Percent(0.29) = %d, want 29", p)
	}
}

func TestGroupForModel(t *testing.T) {
	groups := []QuotaGroup{{Name: "Gemini Models"}, {Name: "Claude and GPT Models"}}
	tests := []struct {
		model  string
		groups []QuotaGroup
		want   int
		ok     bool
	}{
		{"gemini-3.8-flash-high", groups, 0, true},
		{"Gemini 3.8 Flash (High)", groups, 0, true},
		{"claude-opus-4-6-thinking", groups, 0, false},
		{"flash", groups, 0, false},
		{"pro", groups, 0, false},
		{"", groups, 0, false},
		{"gemini-3.8-flash-high", []QuotaGroup{{Name: "Gemini A"}, {Name: "Gemini B"}}, 0, false},
		{"gemini-3.8-flash-high", []QuotaGroup{{Name: "Claude"}}, 0, false},
		{"gemini-3.8-flash-high", nil, 0, false},
	}
	for _, tt := range tests {
		got, ok := groupForModel(tt.groups, tt.model)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("groupForModel(%q) = %d, %v; want %d, %v", tt.model, got, ok, tt.want, tt.ok)
		}
	}
}

func quotaFixture(checked time.Time, remaining float64, reset time.Time) QuotaSnapshot {
	return QuotaSnapshot{CheckedAt: checked, Groups: []QuotaGroup{
		{Name: "Gemini Models", Buckets: []QuotaBucket{{ID: "5h", Name: "5h limit", Window: "5h", RemainingFraction: remaining, ResetTime: reset}}},
		{Name: "Claude and GPT Models", Buckets: []QuotaBucket{{ID: "5h", RemainingFraction: 1}}},
	}}
}

func TestQuotaDecision(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	const model = "gemini-3.8-flash-high"
	tests := []struct {
		name   string
		snap   QuotaSnapshot
		model  string
		refuse bool
	}{
		{"ok passes", quotaFixture(now, 0.9, reset), model, false},
		{"low refuses", quotaFixture(now, 0.2, reset), model, true},
		{"critical refuses", quotaFixture(now, 0.03, reset), model, true},
		{"exhausted refuses", quotaFixture(now, 0, reset), model, true},
		{"unmapped model passes", quotaFixture(now, 0, reset), "claude-x", false},
		{"empty model passes", quotaFixture(now, 0, reset), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refuse, _ := quotaDecision(tt.snap, now, tt.model, testThresholds)
			if refuse != tt.refuse {
				t.Fatalf("refuse = %v, want %v", refuse, tt.refuse)
			}
		})
	}
}

func TestQuotaDecisionTransitions(t *testing.T) {
	t0 := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	reset := t0.Add(30 * time.Minute)
	const model = "gemini-3.8-flash-high"
	decide := func(s QuotaSnapshot, now time.Time) bool {
		r, _ := quotaDecision(s, now, model, testThresholds)
		return r
	}
	low := quotaFixture(t0, 0.03, reset)

	// Past the binding bucket's reset a low snapshot no longer refuses.
	if !decide(low, t0) {
		t.Fatal("low snapshot should refuse before the reset")
	}
	if decide(low, reset.Add(time.Second)) {
		t.Fatal("low snapshot should pass after the binding bucket reset")
	}
	// A new snapshot replaces an ok one.
	ok := quotaFixture(t0, 0.9, reset)
	if decide(ok, t0) {
		t.Fatal("ok snapshot refused")
	}
	if !decide(quotaFixture(t0.Add(time.Minute), 0.03, reset), t0.Add(time.Minute)) {
		t.Fatal("new low snapshot should refuse")
	}
}

// quotaTestManager returns a manager whose probe is stub and whose clock is the
// returned pointer-backed fake.
func quotaTestManager(t *testing.T, interval time.Duration, stub func(context.Context) ([]byte, error)) (*Manager, *atomic.Int64) {
	t.Helper()
	m := New(config.Config{StateDir: t.TempDir(), UsageInterval: interval, QuotaLow: 0.25, QuotaCritical: 0.05})
	base := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	var offset atomic.Int64 // nanoseconds past base
	m.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	m.readUsage = stub
	return m, &offset
}

func okStub(calls *atomic.Int32) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(measuredUsageEnvelope), nil
	}
}

func TestUsageCacheTTLAndMaxAge(t *testing.T) {
	var calls atomic.Int32
	m, off := quotaTestManager(t, 0, okStub(&calls))
	ctx := t.Context()
	if _, err := m.Usage(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	off.Store(int64(30 * time.Second))
	if _, err := m.Usage(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls inside the TTL = %d, want 1", calls.Load())
	}
	off.Store(int64(61 * time.Second))
	if _, err := m.Usage(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls after the TTL = %d, want 2", calls.Load())
	}
	// A 10m max_age accepts a 5m-old snapshot without probing.
	off.Store(int64(61*time.Second + 5*time.Minute))
	if _, err := m.Usage(ctx, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls with max_age 10m = %d, want 2", calls.Load())
	}
	// max_age 0 is raised to the 10s floor, not treated as "always probe".
	if _, err := m.Usage(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls with max_age 0 after 5m = %d, want 3", calls.Load())
	}
	if _, err := m.Usage(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("a second max_age 0 call inside the floor probed again (%d calls)", calls.Load())
	}
}

func TestUsageNegativeCache(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	m, off := quotaTestManager(t, 0, func(context.Context) ([]byte, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, errors.New("agy usage: boom")
		}
		return []byte(measuredUsageEnvelope), nil
	})
	ctx := t.Context()
	if _, err := m.Usage(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	off.Store(int64(2 * time.Minute))
	_, err1 := m.Usage(ctx, time.Minute)
	off.Store(int64(2*time.Minute + 10*time.Second))
	_, err2 := m.Usage(ctx, time.Minute)
	if err1 == nil || err2 == nil || err1.Error() != err2.Error() {
		t.Fatalf("errors = %v, %v; want the same error twice", err1, err2)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (one good, one failed)", calls.Load())
	}
	off.Store(int64(2*time.Minute + 31*time.Second))
	if _, err := m.Usage(ctx, time.Minute); err == nil {
		t.Fatal("expected the failure to persist")
	}
	if calls.Load() != 3 {
		t.Fatalf("calls after the backoff = %d, want 3", calls.Load())
	}
	// The earlier good snapshot is still trusted until the max age.
	if _, ok := m.CachedQuota(); !ok {
		t.Fatal("good snapshot dropped after a failed probe")
	}
	off.Store(int64(11 * time.Minute))
	if _, ok := m.CachedQuota(); ok {
		t.Fatal("snapshot past the max age was still trusted")
	}
}

// TestUsageSingleflight: the caller that starts a probe gives up while the probe
// is blocked, a second caller then finds the same flight in progress instead of
// starting another, and the probe the starter abandoned still completes and
// fills the cache. Every step happens while the stub is blocked, so none of it
// depends on timing: the second caller is observed through the in-flight
// pointer, which a build that does not share the flight would replace.
func TestUsageSingleflight(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var calls atomic.Int32
	// The stub honours its context, so a probe that ran on the starter's
	// context would fail when the starter cancels and leave the cache empty.
	m, _ := quotaTestManager(t, 0, func(ctx context.Context) ([]byte, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return []byte(measuredUsageEnvelope), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	inflight := func() *quotaFlight {
		m.quota.mu.Lock()
		defer m.quota.mu.Unlock()
		return m.quota.inflight
	}

	starterCtx, cancelStarter := context.WithCancel(t.Context())
	starterErr := make(chan error, 1)
	go func() { _, err := m.Usage(starterCtx, time.Minute); starterErr <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never started")
	}
	flight := inflight()
	if flight == nil {
		t.Fatal("no flight in progress while the probe is blocked")
	}

	// The starter gives up while the probe is still blocked: it must return its
	// own context error rather than wait for the probe.
	cancelStarter()
	select {
	case err := <-starterErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("starter error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the starter stayed blocked on the probe after its context was cancelled")
	}

	// A second caller joins the flight in progress. Its context is already
	// cancelled so the call returns at once, but it has by then either joined the
	// flight or replaced it with one of its own.
	gone, cancelGone := context.WithCancel(t.Context())
	cancelGone()
	if _, err := m.Usage(gone, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("second caller error = %v, want context.Canceled", err)
	}
	if inflight() != flight {
		t.Fatal("the second caller started its own probe instead of joining the flight")
	}

	close(release)
	snap, err := m.Usage(t.Context(), time.Minute)
	if err != nil || len(snap.Groups) != 2 {
		t.Fatalf("Usage after the abandoned probe finished = %v, %+v", err, snap)
	}
	if calls.Load() != 1 {
		t.Fatalf("probes = %d, want 1 shared", calls.Load())
	}
}

func TestUsageUnsafeLatch(t *testing.T) {
	var calls atomic.Int32
	m, _ := quotaTestManager(t, time.Minute, func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"conversation_id":"c","status":"SUCCESS","num_turns":1}`), nil
	})
	_, err := m.Usage(t.Context(), time.Minute)
	if !errors.Is(err, errUsageModelTurn) {
		t.Fatalf("first Usage error = %v, want the model-turn latch", err)
	}
	if _, err := m.Usage(t.Context(), time.Minute); !errors.Is(err, errUsageModelTurn) {
		t.Fatalf("second Usage error = %v", err)
	}
	m.CachedQuota()
	done := make(chan struct{})
	go func() { m.runUsageRefresher(t.Context(), time.Minute); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresher kept running after the latch")
	}
	if calls.Load() != 1 {
		t.Fatalf("probes = %d after the latch, want exactly 1", calls.Load())
	}
}

// TestCachedQuotaIsPureRead: CachedQuota never starts a probe, with no snapshot
// or a stale one, even with background probing on; the refresher owns that.
func TestCachedQuotaIsPureRead(t *testing.T) {
	var calls atomic.Int32
	m, off := quotaTestManager(t, time.Minute, okStub(&calls))
	if _, ok := m.CachedQuota(); ok {
		t.Fatal("trusted a snapshot that was never taken")
	}
	if _, err := m.Usage(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	off.Store(int64(5 * time.Minute))
	if _, ok := m.CachedQuota(); !ok {
		t.Fatal("snapshot inside the max age was not trusted")
	}
	off.Store(int64(time.Hour))
	if _, ok := m.CachedQuota(); ok {
		t.Fatal("snapshot past the max age was trusted")
	}
	m.quota.mu.Lock()
	inflight := m.quota.inflight
	m.quota.mu.Unlock()
	if calls.Load() != 1 || inflight != nil {
		t.Fatalf("probes = %d, in flight = %v; CachedQuota must not probe", calls.Load(), inflight != nil)
	}
}

// TestQuotaTrustAgeFollowsInterval pins quotaMaxAge: a snapshot is trusted for
// twice the refresh interval, not for the 10m floor alone, and both the cached
// read and the optional-run guard use that age. With no snapshot at all the
// guard lets the run through.
func TestQuotaTrustAgeFollowsInterval(t *testing.T) {
	var calls atomic.Int32
	m, off := quotaTestManager(t, 30*time.Minute, okStub(&calls))
	req := StartRequest{Priority: PriorityOptional, Model: "gemini-3.8-flash-high"}
	if err := m.checkPriority(req); err != nil {
		t.Fatalf("optional run refused with no snapshot: %v", err)
	}
	m.quota.mu.Lock()
	m.quota.snap = quotaFixture(m.now(), 0.03, m.now().Add(24*time.Hour))
	m.quota.mu.Unlock()
	for _, tc := range []struct {
		at      time.Duration
		trusted bool
	}{
		{45 * time.Minute, true}, // between the interval and twice it
		{59 * time.Minute, true},
		{61 * time.Minute, false}, // past twice the interval
	} {
		off.Store(int64(tc.at))
		if _, ok := m.CachedQuota(); ok != tc.trusted {
			t.Errorf("at %s: CachedQuota trusted = %v, want %v", tc.at, ok, tc.trusted)
		}
		if refused := m.checkPriority(req) != nil; refused != tc.trusted {
			t.Errorf("at %s: optional run refused = %v, want %v", tc.at, refused, tc.trusted)
		}
	}
}

func TestRunUsageRefresher(t *testing.T) {
	var calls atomic.Int32
	m, _ := quotaTestManager(t, 0, okStub(&calls))
	m.runUsageRefresher(t.Context(), 0)
	if calls.Load() != 0 {
		t.Fatal("zero interval probed")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.runUsageRefresher(ctx, 20*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("probes = %d, want an immediate one plus at least one tick", calls.Load())
	}
	cancel()
	<-done
	n := calls.Load()
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != n {
		t.Fatal("refresher probed after its context ended")
	}
}

func TestCheckPriorityRefusalMessage(t *testing.T) {
	var calls atomic.Int32
	m, _ := quotaTestManager(t, 0, okStub(&calls))
	if _, err := m.Usage(t.Context(), time.Minute); err != nil {
		t.Fatal(err)
	}
	err := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: "gemini-3.8-flash-high"})
	if _, ok := errors.AsType[*QuotaRefusalError](err); !ok {
		t.Fatalf("error = %v, want *QuotaRefusalError", err)
	}
	for _, want := range []string{`"Gemini Models"`, "critical", "3%", "2026-10-04T11:32:45Z", "5h limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
	if err := m.checkPriority(StartRequest{Priority: PriorityNormal, Model: "gemini-3.8-flash-high"}); err != nil {
		t.Fatalf("normal run refused: %v", err)
	}
	if err := m.checkPriority(StartRequest{Priority: PriorityOptional, Model: "claude-x"}); err != nil {
		t.Fatalf("optional run on an unmapped model refused: %v", err)
	}
}
