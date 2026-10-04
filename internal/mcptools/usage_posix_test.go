//go:build linux || darwin

package mcptools

import (
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

const geminiTestModel = "gemini-3.8-flash-high"

func quotaFake(remaining float64) testutil.FakeAgy {
	return testutil.FakeAgy{
		Stdout: "REVIEW OK",
		Usage: []testutil.FakeQuotaGroup{
			{Name: "Gemini Models", Buckets: []testutil.FakeQuotaBucket{
				{ID: "5h", Name: "5h limit", Window: "5h", RemainingFraction: remaining, ResetTime: "2099-01-01T00:00:00Z"},
				{ID: "weekly", Name: "Weekly limit", Window: "weekly", RemainingFraction: 0.68, ResetTime: "2099-01-05T00:00:00Z"},
			}},
			{Name: "Claude and GPT Models", Buckets: []testutil.FakeQuotaBucket{
				{ID: "5h", Name: "5h limit", Window: "5h", RemainingFraction: 1, ResetTime: "2099-01-01T00:00:00Z"},
			}},
		},
	}
}

func quotaManagerFor(t *testing.T, remaining float64) (cs *mcp.ClientSession, stateDir string) {
	t.Helper()
	mgr, dir := newTestManagerCfg(t, quotaFake(remaining), func(c *config.Config) {
		c.QuotaLow, c.QuotaCritical = 0.25, 0.05
	})
	return connect(t, mgr, nil), dir
}

func TestAgyUsageOverMCP(t *testing.T) {
	cs, _ := quotaManagerFor(t, 0.03)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "agy_usage"})
	if err != nil || res.IsError {
		t.Fatalf("agy_usage: err=%v res=%+v", err, res)
	}
	groups, _ := structMap(t, res.StructuredContent)["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups = %v", groups)
	}
	g := structMap(t, groups[0])
	if g["level"] != "critical" || g["remaining_percent"] != float64(3) || g["window"] != "5h" {
		t.Fatalf("gemini group = %v", g)
	}
	if got := structMap(t, groups[1])["level"]; got != "ok" {
		t.Fatalf("other group level = %v, want ok", got)
	}
	// An invalid max_age is a tool error, not a probe.
	res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "agy_usage", Arguments: map[string]any{"max_age": "soon"}})
	if err != nil || !res.IsError {
		t.Fatalf("invalid max_age: err=%v res=%+v, want a tool error", err, res)
	}
}

func TestRunResultsCarryQuota(t *testing.T) {
	cs, _ := quotaManagerFor(t, 0.5)
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s: err=%v res=%+v", name, err, res)
		}
		return structMap(t, res.StructuredContent)
	}
	hasQuota := func(sc map[string]any) bool {
		q, ok := sc["quota"].(map[string]any)
		if !ok {
			return false
		}
		g, _ := q["groups"].([]any)
		return len(g) == 2
	}

	// Empty cache and background probing off (interval 0): quota is absent.
	first := call("agy_run_sync", map[string]any{"prompt": "x", "wait": "30s"})
	if _, present := first["quota"]; present {
		t.Fatalf("quota present without a snapshot: %v", first["quota"])
	}

	call("agy_usage", nil)
	sync := call("agy_run_sync", map[string]any{"prompt": "x", "wait": "30s"})
	if !hasQuota(sync) {
		t.Fatalf("agy_run_sync result lacks quota: %v", sync)
	}
	id, _ := sync["job_id"].(string)
	if !hasQuota(call("agy_status", map[string]any{"job_id": id})) {
		t.Fatal("agy_status result lacks quota")
	}
	if !hasQuota(call("agy_wait", map[string]any{"job_id": id, "wait": "30s"})) {
		t.Fatal("agy_wait result lacks quota")
	}
	if !hasQuota(call("agy_run", map[string]any{"prompt": "x"})) {
		t.Fatal("agy_run result lacks quota")
	}
}

func TestOptionalRunRefusedOverMCP(t *testing.T) {
	cs, stateDir := quotaManagerFor(t, 0.03)
	if res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "agy_usage"}); err != nil || res.IsError {
		t.Fatalf("agy_usage: err=%v res=%+v", err, res)
	}
	args := map[string]any{"prompt": "review", "model": geminiTestModel, "priority": "optional", "wait": "30s"}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "agy_run_sync", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("optional run was not refused: %+v", res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"Gemini Models", "critical", "3%", "2099-01-01T00:00:00Z"} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal %q lacks %q", text, want)
		}
	}
	if entries, _ := os.ReadDir(stateDir); len(entries) != 0 {
		t.Fatalf("state dir holds %d entries after a refusal, want none", len(entries))
	}

	args["priority"] = "normal"
	res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "agy_run_sync", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("normal run: err=%v res=%+v", err, res)
	}
	if id, _ := structMap(t, res.StructuredContent)["job_id"].(string); id == "" {
		t.Fatal("normal run returned no job id")
	}
}

func TestOptionalRunUnmappedModelRuns(t *testing.T) {
	cs, _ := quotaManagerFor(t, 0.03)
	if res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "agy_usage"}); err != nil || res.IsError {
		t.Fatalf("agy_usage: err=%v res=%+v", err, res)
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "agy_run_sync",
		Arguments: map[string]any{"prompt": "x", "model": "claude-x", "priority": "optional", "wait": "30s"}})
	if err != nil || res.IsError {
		t.Fatalf("optional run on an unmapped model: err=%v res=%+v", err, res)
	}
}
