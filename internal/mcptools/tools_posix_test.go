//go:build linux || darwin

package mcptools

import (
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// This file is posix-gated (linux || darwin): the tests drive StartJob, which
// only runs job supervision on Linux and macOS. The cross-platform tool tests
// (toStartRequest validation, HTTP serving) live in tools_test.go /
// serve_http_test.go so `go test ./...` stays green on Windows too.

// TestListModelsOverMCP exercises the list_models tool end to end: the handler
// decodes the JSON envelope from `agy --output-format json models` (the fake
// renders three {id,label} entries, the last label-less) and returns them on the
// wire. This handler had no MCP-layer test.
//
// The split matters, it is not cosmetic: `models` is what a caller passes as the
// model parameter, so it must carry ids alone. Returning the whole row, or the
// label, is issue #135, because a label makes agy refuse any run that also sets
// effort.
func TestListModelsOverMCP(t *testing.T) {
	// The third entry carries no label, so this also pins what a label-less model
	// looks like on the wire: an empty string, not a missing key.
	mgr, _ := newTestManager(t, testutil.FakeAgy{
		Models: []testutil.FakeModel{
			{ID: "gemini-3.1-pro-high", Label: "Gemini 3.1 Pro (High)"},
			{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6 (Thinking)"},
			{ID: "some-unlabelled-model"},
		},
	})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_models"})
	if err != nil || res.IsError {
		t.Fatalf("list_models: err=%v res=%+v", err, res)
	}
	out := structMap(t, res.StructuredContent)
	wantIDs := []string{"gemini-3.1-pro-high", "claude-sonnet-4-6", "some-unlabelled-model"}
	wantLabels := []string{"Gemini 3.1 Pro (High)", "Claude Sonnet 4.6 (Thinking)", ""}

	models, _ := out["models"].([]any)
	if len(models) != len(wantIDs) {
		t.Fatalf("models = %v, want %d ids", models, len(wantIDs))
	}
	details, _ := out["model_details"].([]any)
	if len(details) != len(wantIDs) {
		t.Fatalf("model_details = %v, want one entry per model", details)
	}
	// Assert every entry, not just the first: the schema promises model_details is
	// "in the same order as models", and a mispairing that starts at entry 2 is
	// exactly what a spot-check of entry 0 cannot see.
	for i := range wantIDs {
		if models[i] != wantIDs[i] {
			t.Errorf("models[%d] = %v, want the id column alone (%q)", i, models[i], wantIDs[i])
		}
		d, _ := details[i].(map[string]any)
		if d["id"] != wantIDs[i] || d["label"] != wantLabels[i] {
			t.Errorf("model_details[%d] = %v, want id %q paired with label %q", i, d, wantIDs[i], wantLabels[i])
		}
	}
}

// TestListModelsEmptyCatalogReturnsArrays: with nothing to list, both fields must
// be empty JSON arrays rather than null, so a client can index the result without
// a null check. The handler builds both with make(), and nothing else pins that;
// dropping either initialiser marshals the field as null and breaks such a client
// silently.
func TestListModelsEmptyCatalogReturnsArrays(t *testing.T) {
	// No Models configured: the fake emits an envelope with an empty models array,
	// which the handler must turn into empty wire arrays, not null.
	mgr, _ := newTestManager(t, testutil.FakeAgy{})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_models"})
	if err != nil || res.IsError {
		t.Fatalf("list_models: err=%v res=%+v", err, res)
	}
	out := structMap(t, res.StructuredContent)
	for _, field := range []string{"models", "model_details"} {
		// A null decodes to nil any, which fails this assertion; an empty array
		// decodes to an empty []any, which passes.
		got, ok := out[field].([]any)
		if !ok {
			t.Errorf("%s = %v, want an empty array, not null", field, out[field])
			continue
		}
		if len(got) != 0 {
			t.Errorf("%s = %v, want empty for an empty catalog", field, got)
		}
	}
}

// TestListAgentsOverMCP exercises the list_agents tool end to end: the handler
// decodes the JSON envelope from `agy --output-format json agents` (the fake
// renders two plain names) and returns them on the wire, in order. Unlike models
// there is no id/label split, because agy takes an agent name verbatim.
func TestListAgentsOverMCP(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Agents: []string{"reviewer", "researcher"}})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_agents"})
	if err != nil || res.IsError {
		t.Fatalf("list_agents: err=%v res=%+v", err, res)
	}
	out := structMap(t, res.StructuredContent)
	want := []string{"reviewer", "researcher"}
	agents, _ := out["agents"].([]any)
	if len(agents) != len(want) {
		t.Fatalf("agents = %v, want %d names", agents, len(want))
	}
	for i := range want {
		if agents[i] != want[i] {
			t.Errorf("agents[%d] = %v, want %q", i, agents[i], want[i])
		}
	}
}

// TestListAgentsEmptyCatalogReturnsArray: no agents configured is the common
// state, so the field must be an empty JSON array rather than null, letting a
// client index the result without a null check. The handler forces a non-nil
// slice; dropping that marshals the field as null and breaks such a client.
func TestListAgentsEmptyCatalogReturnsArray(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_agents"})
	if err != nil || res.IsError {
		t.Fatalf("list_agents: err=%v res=%+v", err, res)
	}
	out := structMap(t, res.StructuredContent)
	got, ok := out["agents"].([]any)
	if !ok {
		t.Fatalf("agents = %v, want an empty array, not null", out["agents"])
	}
	if len(got) != 0 {
		t.Errorf("agents = %v, want empty for an empty catalog", got)
	}
}

// TestListSessionsOverMCP exercises the list_sessions tool: with an empty cache
// the handler must return a non-nil empty array on the wire (not null), and the
// tool must be registered and wired. This handler had no MCP-layer test.
func TestListSessionsOverMCP(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "x"})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_sessions"})
	if err != nil || res.IsError {
		t.Fatalf("list_sessions: err=%v res=%+v", err, res)
	}
	sessions, ok := structMap(t, res.StructuredContent)["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions field is not an array: %v", res.StructuredContent)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions = %v, want empty for an empty cache", sessions)
	}
}

// agy_run is the one tool that spends the conversation-id budget: it returns
// before the run has produced anything a caller could read the id from, so
// without the wait a fresh run's conversation_id would come back empty and the
// caller would have to poll for it. This is the only test that pins that, and the
// shared helper leaves the budget at zero, so dropping the call would otherwise
// go unnoticed.
func TestAgyRunReportsFreshConversationIDWithinBudget(t *testing.T) {
	const uuid = "13131313-2424-3535-4646-575757575757"
	// Sleeps past the assertion, so the id has to come from the progress file of a
	// job that is still running, not from a finished one's result.
	mgr, stateDir := newTestManagerWithIDWait(t, testutil.FakeAgy{
		Stdout: "LATE OK", Exit: 0, Sleep: 20 * time.Second, ConversationID: uuid,
	}, 30*time.Second)
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run",
		Arguments: map[string]any{"prompt": "review"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run: err=%v res=%+v", err, res)
	}
	sc := structMap(t, res.StructuredContent)
	jobID, _ := sc["job_id"].(string)
	if jobID == "" {
		t.Fatal("empty job id")
	}
	killJobGroup(t, stateDir, jobID)
	if sc["conversation_id"] != uuid {
		t.Fatalf("conversation_id = %v, want %q from the id the supervisor recorded", sc["conversation_id"], uuid)
	}
}

func TestAgyRunAndStatusOverMCP(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "REVIEW OK", Exit: 0})
	cs := connect(t, mgr, nil)
	ctx := t.Context()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "agy_run",
		Arguments: map[string]any{"prompt": "review"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run: err=%v res=%+v", err, res)
	}
	jobID, _ := structMap(t, res.StructuredContent)["job_id"].(string)
	if jobID == "" {
		t.Fatal("empty job id")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name: "agy_status", Arguments: map[string]any{"job_id": jobID}})
		if err != nil {
			t.Fatal(err)
		}
		sc := structMap(t, s.StructuredContent)
		if sc["state"] == manager.StateDone {
			if sc["result"] != "REVIEW OK" {
				t.Fatalf("result = %v", sc["result"])
			}
			return
		}
		// Fail fast on a terminal failure/cancel instead of burning the deadline
		// and reporting an opaque timeout.
		if state := sc["state"]; state == manager.StateFailed || state == manager.StateCancelled {
			t.Fatalf("job reached terminal %v, want done; error=%v", state, sc["error"])
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not reach done")
}
