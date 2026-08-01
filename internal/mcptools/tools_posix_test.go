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
