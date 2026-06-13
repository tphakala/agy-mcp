package mcptools

import (
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// TestToStartRequestRejectsExcessiveTimeout: a client timeout is validated
// positive but must also be capped, so a typo like "1000h" cannot become both
// the agy --print-timeout and a weeks-long supervisor hard-kill window.
func TestToStartRequestRejectsExcessiveTimeout(t *testing.T) {
	_, err := runInput{Prompt: "x", Timeout: "1000h"}.toStartRequest()
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("err = %v, want an excessive-timeout rejection", err)
	}
}

func TestToStartRequestAcceptsTimeoutAtLimit(t *testing.T) {
	req, err := runInput{Prompt: "x", Timeout: maxJobTimeout.String()}.toStartRequest()
	if err != nil || req.Timeout != maxJobTimeout {
		t.Fatalf("timeout at the limit should be accepted: req=%+v err=%v", req, err)
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
