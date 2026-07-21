package mcptools

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// listRegisteredTools returns the tools/list payload a real client receives, so
// these assertions run against the wire shape rather than the registration
// literals. NewServer takes a nil manager: listing tools never dispatches a
// handler, so no manager is needed.
func listRegisteredTools(t *testing.T) []*mcp.Tool {
	t.Helper()
	ctx := t.Context()
	ct, st := mcp.NewInMemoryTransports()
	srv := NewServer(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Run(ctx, st)
	}()
	t.Cleanup(func() { <-done })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "toolquality-test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	return res.Tools
}

// TestToolDefinitionsDeclareAnnotations: absent annotations are not neutral. The
// MCP spec defaults them to destructiveHint=true, openWorldHint=true,
// readOnlyHint=false, so a tool that ships none advertises itself as a
// destructive open-world mutation. Every tool must state its real behaviour, and
// the read-only ones must say so explicitly or clients keep assuming the worst.
func TestToolDefinitionsDeclareAnnotations(t *testing.T) {
	readOnly := map[string]bool{
		toolAgyStatus:    true,
		toolAgyWait:      true,
		toolListModels:   true,
		toolListSessions: true,
		toolAgyRun:       false,
		toolAgyRunSync:   false,
		toolAgyCancel:    false,
	}
	for _, tool := range listRegisteredTools(t) {
		t.Run(tool.Name, func(t *testing.T) {
			if tool.Annotations == nil {
				t.Fatal("no annotations: clients will assume destructive + open-world")
			}
			want, known := readOnly[tool.Name]
			if !known {
				t.Fatalf("tool %q is not classified here; add it to the readOnly map", tool.Name)
			}
			if tool.Annotations.ReadOnlyHint != want {
				t.Errorf("readOnlyHint = %v, want %v", tool.Annotations.ReadOnlyHint, want)
			}
			if tool.Annotations.OpenWorldHint == nil {
				t.Error("openWorldHint unset; it defaults to true, so state it explicitly")
			}
			// destructiveHint and idempotentHint are meaningful only when
			// readOnlyHint is false, so require them exactly where they carry meaning.
			if !want && tool.Annotations.DestructiveHint == nil {
				t.Error("mutating tool must state destructiveHint")
			}
			if want && tool.Annotations.DestructiveHint != nil {
				t.Error("read-only tool should not carry a decorative destructiveHint")
			}
		})
	}
}

// TestToolDefinitionsDescribeThemselves guards the qualities a tool description
// is actually judged on: it must exist, must not merely restate the tool's own
// name, and must point at the sibling tools an agent could otherwise confuse it
// with. Every input property needs its own description too, since that is where
// parameter meaning belongs rather than duplicated into the prose.
func TestToolDefinitionsDescribeThemselves(t *testing.T) {
	// Each tool names at least one sibling, so an agent reading one definition
	// learns when to reach for a different one instead.
	wantSiblings := map[string][]string{
		toolAgyRun:       {toolAgyRunSync, toolAgyWait, toolAgyStatus},
		toolAgyRunSync:   {toolAgyRun, toolAgyWait, toolAgyStatus},
		toolAgyWait:      {toolAgyRun, toolAgyStatus},
		toolAgyStatus:    {toolAgyWait},
		toolAgyCancel:    {toolAgyRun},
		toolListModels:   {toolAgyRun, toolAgyRunSync},
		toolListSessions: {toolAgyRun, toolAgyRunSync},
	}
	for _, tool := range listRegisteredTools(t) {
		t.Run(tool.Name, func(t *testing.T) {
			if tool.Title == "" {
				t.Error("no title")
			}
			desc := strings.TrimSpace(tool.Description)
			if desc == "" {
				t.Fatal("no description")
			}
			// A description that only restates the name (bare, spaced, or titled)
			// tells an agent nothing the tool list already showed it.
			for _, tautology := range []string{
				tool.Name,
				strings.ReplaceAll(tool.Name, "_", " "),
				tool.Title,
			} {
				if strings.EqualFold(strings.TrimSuffix(desc, "."), tautology) {
					t.Errorf("description merely restates %q", tautology)
				}
			}
			for _, sibling := range wantSiblings[tool.Name] {
				if !strings.Contains(desc, sibling) {
					t.Errorf("description never mentions sibling %q, so an agent cannot route between them", sibling)
				}
			}
			schema, ok := tool.InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("input schema has unexpected type %T", tool.InputSchema)
			}
			props, _ := schema["properties"].(map[string]any)
			for name, raw := range props {
				prop, ok := raw.(map[string]any)
				if !ok {
					t.Errorf("property %q has unexpected type %T", name, raw)
					continue
				}
				if d, _ := prop["description"].(string); strings.TrimSpace(d) == "" {
					t.Errorf("property %q has no description", name)
				}
			}
		})
	}
}
