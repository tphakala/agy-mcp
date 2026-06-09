package mcptools

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func writeFakeSupervisor(t *testing.T, agy string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-sup")
	// Mimics `agy-mcp run-job <dir>`: runs the fake agy, captures stdout to out,
	// writes exit_code. Sets its comm to the script basename so the manager's
	// liveness check (which compares /proc/<pid>/comm against the supervisor
	// basename) sees it as alive while it runs, like the real supervisor.
	script := "#!/usr/bin/env bash\n" +
		"printf 'fake-sup' > /proc/$$/comm\n" +
		"dir=\"$2\"\n" +
		"\"" + agy + "\" -p x > \"$dir/out\" 2> \"$dir/err\"\n" +
		"printf '%s' \"$?\" > \"$dir/exit_code\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAgyRunAndStatusOverMCP(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "REVIEW OK", Exit: 0})
	sup := writeFakeSupervisor(t, agy)
	c := config.Config{AgyPath: agy, SupervisorExe: sup, StateDir: t.TempDir(),
		DefaultTimeout: time.Minute, MaxConcurrency: 4}
	mgr := manager.New(c)

	srv := NewServer(mgr)
	ct, st := mcp.NewInMemoryTransports()
	ctx := t.Context()
	if _, err := srv.Connect(ctx, st, nil); err != nil { // server side
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

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
		if sc["state"] == "done" {
			if sc["result"] != "REVIEW OK" {
				t.Fatalf("result = %v", sc["result"])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not reach done")
}
