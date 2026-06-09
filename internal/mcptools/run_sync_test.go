package mcptools

import (
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

// connect wires a NewServer(mgr) to a fresh client over in-memory transports
// and returns the client session.
func connect(t *testing.T, mgr *manager.Manager, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	srv := NewServer(mgr)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(t.Context(), st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, opts)
	cs, err := client.Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// newTestManager builds a manager around a fake agy and fake supervisor.
func newTestManager(t *testing.T, fake testutil.FakeAgy) (*manager.Manager, string) {
	t.Helper()
	agy := testutil.WriteFakeAgy(t, fake)
	sup := writeFakeSupervisor(t, agy)
	stateDir := t.TempDir()
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: stateDir,
		DefaultTimeout: time.Minute, MaxConcurrency: 4}
	return manager.New(c), stateDir
}

func TestAgyRunSyncCompletesInline(t *testing.T) {
	mgr, _ := newTestManager(t, testutil.FakeAgy{Stdout: "REVIEW OK", Exit: 0})
	cs := connect(t, mgr, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "agy_run_sync",
		Arguments: map[string]any{"prompt": "review", "wait": "30s"},
	})
	if err != nil || res.IsError {
		t.Fatalf("agy_run_sync: err=%v res=%+v", err, res)
	}
	sc := res.StructuredContent.(map[string]any)
	if sc["state"] != "done" {
		t.Fatalf("state = %v, want done", sc["state"])
	}
	if sc["result"] != "REVIEW OK" {
		t.Fatalf("result = %v", sc["result"])
	}
	if id, _ := sc["job_id"].(string); id == "" {
		t.Fatal("empty job id")
	}
}
