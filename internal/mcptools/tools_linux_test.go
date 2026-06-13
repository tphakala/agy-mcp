package mcptools

import (
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// This file is _linux-gated: the tests drive StartJob, which only runs job
// supervision on Linux. The cross-platform tool tests (toStartRequest
// validation, HTTP serving) live in tools_test.go / serve_http_test.go so
// `go test ./...` stays green on macOS and Windows.

// TestListModelsOverMCP exercises the list_models tool end to end: the handler
// runs `agy models` (the fake agy prints two lines) and returns them on the
// wire. This handler had no MCP-layer test.
func TestListModelsOverMCP(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "Model A\nModel B"})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_models"})
	if err != nil || res.IsError {
		t.Fatalf("list_models: err=%v res=%+v", err, res)
	}
	models, _ := structMap(t, res.StructuredContent)["models"].([]any)
	if len(models) != 2 || models[0] != "Model A" || models[1] != "Model B" {
		t.Fatalf("models = %v, want [Model A, Model B]", models)
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
