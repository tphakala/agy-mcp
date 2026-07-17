//go:build linux || darwin

package mcptools

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func TestAgyWaitReturnsResult(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "WAITED OK", Exit: 0, Sleep: 500 * time.Millisecond})
	cs := connect(t, mgr, nil)

	runRes, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run",
		Arguments: map[string]any{"prompt": "review"},
	})
	if err != nil || runRes.IsError {
		t.Fatalf("agy_run: err=%v res=%+v", err, runRes)
	}
	jobID, _ := structMap(t, runRes.StructuredContent)["job_id"].(string)
	if jobID == "" {
		t.Fatal("empty job id")
	}

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_wait",
		Arguments: map[string]any{"job_id": jobID, "wait": "30s"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_wait: err=%v res=%+v", err, res)
	}
	sc := structMap(t, res.StructuredContent)
	if sc["state"] != manager.StateDone {
		t.Fatalf("state = %v, want done", sc["state"])
	}
	if sc["result"] != "WAITED OK" {
		t.Fatalf("result = %v, want WAITED OK", sc["result"])
	}
	if sc["job_id"] != jobID {
		t.Fatalf("job_id = %v, want %q", sc["job_id"], jobID)
	}
}

func TestAgyWaitUnknownJob(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "x", Exit: 0})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_wait",
		Arguments: map[string]any{"job_id": "no-such-job"},
	})
	if err == nil && !res.IsError {
		t.Fatalf("expected an error for an unknown job, got %+v", res)
	}
}

// TestAgyWaitInvalidWait proves the invalid-wait error comes from parseWait,
// not from an unknown job id. It targets a real, observably running job with
// each invalid wait: if the implementation looked the job up before
// validating wait, a real job id would let the call through, so this only
// passes when wait is rejected first.
func TestAgyWaitInvalidWait(t *testing.T) {
	mgr, stateDir := newTestManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 2 * time.Second})
	cs := connect(t, mgr, nil)

	runRes, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run",
		Arguments: map[string]any{"prompt": "review"},
	})
	if err != nil || runRes.IsError {
		t.Fatalf("agy_run: err=%v res=%+v", err, runRes)
	}
	jobID, _ := structMap(t, runRes.StructuredContent)["job_id"].(string)
	if jobID == "" {
		t.Fatal("empty job id")
	}
	// Confirm the job is actually running before probing it, so a pass here
	// cannot be explained by the job not existing yet either.
	waitForRunningJob(t, mgr, stateDir, 5*time.Second)

	for _, wait := range []string{"nope", "-1s", "0"} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
			Name:      "agy_wait",
			Arguments: map[string]any{"job_id": jobID, "wait": wait},
		})
		if err == nil && !res.IsError {
			t.Errorf("wait %q: expected an error, got %+v", wait, res.StructuredContent)
			continue
		}
		text := callToolErrorText(err, res)
		if !strings.Contains(text, "invalid wait") {
			t.Errorf("wait %q: error = %q, want it to contain %q", wait, text, "invalid wait")
		}
	}

	waitForDone(t, mgr, jobID, "OK", 15*time.Second)
}

// callToolErrorText extracts the human-readable error message from a failed
// CallTool response, whichever of the two error surfaces the SDK used: a
// transport-level err (context or protocol failure), or a tool-level error
// result whose message lands in the first text content block.
func callToolErrorText(err error, res *mcp.CallToolResult) string {
	if err != nil {
		return err.Error()
	}
	if res != nil {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				return tc.Text
			}
		}
	}
	return ""
}

func TestAgyWaitOverrunReturnsNote(t *testing.T) {
	// The sleep must comfortably outlast the 100ms wait cap even on a stalled
	// CI runner, or the job finishes before the cap and the running assertion
	// flakes.
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "LATE OK", Exit: 0, Sleep: 2 * time.Second})
	cs := connect(t, mgr, nil)

	runRes, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run",
		Arguments: map[string]any{"prompt": "review"},
	})
	if err != nil || runRes.IsError {
		t.Fatalf("agy_run: err=%v res=%+v", err, runRes)
	}
	jobID, _ := structMap(t, runRes.StructuredContent)["job_id"].(string)
	if jobID == "" {
		t.Fatal("empty job id")
	}

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_wait",
		Arguments: map[string]any{"job_id": jobID, "wait": "100ms"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_wait: err=%v res=%+v", err, res)
	}
	sc := structMap(t, res.StructuredContent)
	if sc["state"] != manager.StateRunning {
		t.Fatalf("state = %v, want running", sc["state"])
	}
	if note, _ := sc["note"].(string); note == "" {
		t.Fatal("expected an overrun note")
	}

	// The overrun must not have cancelled the job: it finishes on its own.
	waitForDone(t, mgr, jobID, "LATE OK", 15*time.Second)
}

func TestAgyWaitSendsProgress(t *testing.T) {
	// One second spans several 250ms poll ticks, so at least one progress
	// notification fires while the job runs.
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "OK", Exit: 0, Sleep: 1 * time.Second})

	var mu sync.Mutex
	var tokens []any
	opts := &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			tokens = append(tokens, r.Params.ProgressToken)
			mu.Unlock()
		},
	}
	cs := connect(t, mgr, opts)

	runRes, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run",
		Arguments: map[string]any{"prompt": "review"},
	})
	if err != nil || runRes.IsError {
		t.Fatalf("agy_run: err=%v res=%+v", err, runRes)
	}
	jobID, _ := structMap(t, runRes.StructuredContent)["job_id"].(string)
	if jobID == "" {
		t.Fatal("empty job id")
	}

	params := &mcp.CallToolParams{
		Name:      "agy_wait",
		Arguments: map[string]any{"job_id": jobID, "wait": "30s"},
	}
	params.SetProgressToken("tok-9")
	res, err := cs.CallTool(t.Context(), params)
	if err != nil || res.IsError {
		t.Fatalf("agy_wait: err=%v res=%+v", err, res)
	}
	if sc := structMap(t, res.StructuredContent); sc["state"] != manager.StateDone {
		t.Fatalf("state = %v, want done", sc["state"])
	}

	// Notifications are one-way; give in-flight ones a moment to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(tokens)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) == 0 {
		t.Fatal("no progress notifications received")
	}
	for _, tok := range tokens {
		if tok != "tok-9" {
			t.Fatalf("progress token = %v, want tok-9", tok)
		}
	}
}
