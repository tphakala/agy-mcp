package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServeIntrospectsWithoutAgy is the end-to-end guarantee behind the deferred
// lookup: the real binary, started with no agy reachable anywhere on PATH, must
// still complete initialize and answer tools/list.
//
// This is what a client sees before it ever runs a prompt, and what an automated
// directory check sees in a container that has only the agy-mcp binary in it.
// Refusing to start there made the server undiscoverable over a dependency that
// introspection never touches.
func TestServeIntrospectsWithoutAgy(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin)
	// An empty PATH plus a state dir of our own, so the server cannot find agy and
	// cannot touch the developer's real job store. HOME/USERPROFILE are pinned too:
	// the XDG fallback reads them when AGY_MCP_STATE_DIR is unset, and this run
	// must not depend on the ambient home either way.
	home := t.TempDir()
	cmd.Env = append(os.Environ(),
		"PATH="+t.TempDir(),
		"AGY_MCP_AGY_PATH=",
		"AGY_MCP_STATE_DIR="+t.TempDir(),
		"HOME="+home,
		"USERPROFILE="+home,
	)

	client := mcp.NewClient(&mcp.Implementation{Name: "introspect", Version: "0"}, nil)
	ctx := t.Context()
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("initialize must succeed without agy on PATH: %v", err)
	}
	defer func() { _ = cs.Close() }()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list must succeed without agy on PATH: %v", err)
	}
	got := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{"agy_run", "agy_run_sync", "agy_status", "agy_wait", "agy_cancel", "list_models", "list_sessions"} {
		if !got[name] {
			t.Errorf("tool %q missing from a server started without agy", name)
		}
	}
}

// TestServeWarnsWhenAgyMissing: tolerating a missing agy must not be silent.
// Starting cleanly and then failing every run with no hint at startup is worse
// for an operator than the old hard refusal was, so the server says so once, on
// stderr (stdout is the JSON-RPC stream and must stay clean).
func TestServeWarnsWhenAgyMissing(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}

	stderrPath := filepath.Join(t.TempDir(), "stderr.log")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stderr.Close() }()

	cmd := exec.Command(bin)
	cmd.Stderr = stderr
	home := t.TempDir()
	cmd.Env = append(os.Environ(),
		"PATH="+t.TempDir(),
		"AGY_MCP_AGY_PATH=",
		"AGY_MCP_STATE_DIR="+t.TempDir(),
		"HOME="+home,
		"USERPROFILE="+home,
	)

	client := mcp.NewClient(&mcp.Implementation{Name: "introspect", Version: "0"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Close before reading: the warning is emitted during startup, so a completed
	// initialize means it has already been written, and closing flushes the pipe.
	_ = cs.Close()

	logged, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "agy not found on PATH") {
		t.Errorf("startup must warn that agy is missing; stderr was:\n%s", logged)
	}
}

// TestRunWithoutAgyFailsPerCall is the other half of the contract: tolerating a
// missing agy at startup must not turn into a silent no-op later. The tool call
// that actually needs the binary has to say so, and name the way to fix it.
func TestRunWithoutAgyFailsPerCall(t *testing.T) {
	bin, err := buildBinary()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin)
	home := t.TempDir()
	cmd.Env = append(os.Environ(),
		"PATH="+t.TempDir(),
		"AGY_MCP_AGY_PATH=",
		"AGY_MCP_STATE_DIR="+t.TempDir(),
		"HOME="+home,
		"USERPROFILE="+home,
	)

	client := mcp.NewClient(&mcp.Implementation{Name: "introspect", Version: "0"}, nil)
	ctx := t.Context()
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_models",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call list_models: %v", err)
	}
	if !res.IsError {
		t.Fatal("list_models must report an error when agy cannot be resolved")
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "AGY_MCP_AGY_PATH") {
		t.Errorf("error should point at the fix, got: %s", text.String())
	}
}
