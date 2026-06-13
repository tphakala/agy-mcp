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
	jobID := res.StructuredContent.(map[string]any)["job_id"].(string)
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
		sc := s.StructuredContent.(map[string]any)
		if sc["state"] == manager.StateDone {
			if sc["result"] != "REVIEW OK" {
				t.Fatalf("result = %v", sc["result"])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not reach done")
}
