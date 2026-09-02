package mcptools

import (
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// listRegisteredTools returns the tools/list payload a real client receives, so
// these assertions run against the wire shape rather than the registration
// literals. connect takes a nil manager: listing tools never dispatches a
// handler, so no manager is needed.
func listRegisteredTools(t *testing.T) []*mcp.Tool {
	t.Helper()
	res, err := connect(t, nil, nil).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	return res.Tools
}

// wantAnnotations is the expected annotation for every registered tool, written
// out field by field rather than as "which shared var was used". Asserting the
// values (not merely that some annotation is present) is the point: the choice
// between destructive and read-only, and between an open and closed world, is
// the claim this server makes to its clients, so a silent flip must fail here.
var wantAnnotations = map[string]mcp.ToolAnnotations{
	toolAgyRun:       {DestructiveHint: new(true), OpenWorldHint: new(true)},
	toolAgyRunSync:   {DestructiveHint: new(true), OpenWorldHint: new(true)},
	toolAgyCancel:    {DestructiveHint: new(true), IdempotentHint: true, OpenWorldHint: new(false)},
	toolAgyStatus:    {ReadOnlyHint: true, OpenWorldHint: new(false)},
	toolAgyWait:      {ReadOnlyHint: true, OpenWorldHint: new(false)},
	toolListSessions: {ReadOnlyHint: true, OpenWorldHint: new(false)},
	toolListModels:   {ReadOnlyHint: true, OpenWorldHint: new(true)},
	toolListAgents:   {ReadOnlyHint: true, OpenWorldHint: new(true)},
}

// eqBoolPtr compares two optional hints. Nil is distinct from false: the MCP
// spec defaults destructiveHint and openWorldHint to true, so "unset" and
// "explicitly false" say different things on the wire.
func eqBoolPtr(got, want *bool) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

// TestToolDefinitionsDeclareAnnotations: absent annotations are not neutral. The
// MCP spec defaults them to destructiveHint=true, openWorldHint=true,
// readOnlyHint=false, so a tool that ships none advertises itself as a
// destructive open-world mutation. Every tool must state its real behaviour.
func TestToolDefinitionsDeclareAnnotations(t *testing.T) {
	tools := listRegisteredTools(t)
	// A tool registered but never classified here would otherwise be reviewed by
	// nothing, so require the two sets to match exactly in both directions.
	if len(tools) != len(wantAnnotations) {
		t.Errorf("got %d tools, want %d; a tool was added or removed without updating wantAnnotations", len(tools), len(wantAnnotations))
	}
	for _, tool := range tools {
		t.Run(tool.Name, func(t *testing.T) {
			want, known := wantAnnotations[tool.Name]
			if !known {
				t.Fatalf("tool %q is not classified here; add it to wantAnnotations", tool.Name)
			}
			got := tool.Annotations
			if got == nil {
				t.Fatal("no annotations: clients will assume destructive + open-world")
			}
			if got.ReadOnlyHint != want.ReadOnlyHint {
				t.Errorf("readOnlyHint = %v, want %v", got.ReadOnlyHint, want.ReadOnlyHint)
			}
			if got.IdempotentHint != want.IdempotentHint {
				t.Errorf("idempotentHint = %v, want %v", got.IdempotentHint, want.IdempotentHint)
			}
			if !eqBoolPtr(got.DestructiveHint, want.DestructiveHint) {
				t.Errorf("destructiveHint = %v, want %v", fmtHint(got.DestructiveHint), fmtHint(want.DestructiveHint))
			}
			if !eqBoolPtr(got.OpenWorldHint, want.OpenWorldHint) {
				t.Errorf("openWorldHint = %v, want %v", fmtHint(got.OpenWorldHint), fmtHint(want.OpenWorldHint))
			}
			// destructiveHint and idempotentHint are meaningful only when
			// readOnlyHint is false, so a read-only tool carrying either is stating
			// something the spec says nothing reads.
			if want.ReadOnlyHint && got.DestructiveHint != nil {
				t.Error("read-only tool should not carry a decorative destructiveHint")
			}
		})
	}
}

// fmtHint renders an optional hint so a failure distinguishes unset from false.
func fmtHint(b *bool) string {
	if b == nil {
		return "unset"
	}
	if *b {
		return "true"
	}
	return "false"
}

// wantSiblings names, per tool, the other tools its description must mention, so
// an agent reading one definition learns when to reach for a different one.
var wantSiblings = map[string][]string{
	toolAgyRun:     {toolAgyRunSync, toolAgyWait, toolAgyStatus},
	toolAgyRunSync: {toolAgyRun, toolAgyWait, toolAgyStatus},
	toolAgyWait:    {toolAgyRun, toolAgyStatus},
	// agy_status routes to agy_wait (the blocking alternative) and no further:
	// requiring it to name agy_cancel too bought nothing an agent needs at a
	// status check, and the first attempt to satisfy that invented a claim about
	// polling cost that the code did not support.
	toolAgyStatus: {toolAgyWait},
	// agy_cancel routes to agy_status as well as agy_run: a cancelled job can
	// still carry a complete answer (agy prints its result and can hang on the
	// way out), so an agent that re-sends the prompt without collecting it first
	// pays for the same work twice.
	toolAgyCancel:    {toolAgyRun, toolAgyStatus},
	toolListModels:   {toolAgyRun, toolAgyRunSync},
	toolListAgents:   {toolAgyRun, toolAgyRunSync},
	toolListSessions: {toolAgyRun, toolAgyRunSync},
}

// mentions reports whether desc names tool as a whole word. A plain substring
// test would let "agy_run_sync" satisfy a requirement to mention "agy_run",
// which silently hollows out the cross-reference check for every tool whose
// description happens to name the sync variant.
func mentions(desc, tool string) bool {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(tool) + `\b`).MatchString(desc)
}

// TestToolDefinitionsDescribeThemselves guards the qualities a tool description
// is judged on: it must exist, must not merely restate the tool's own name, and
// must route to the sibling tools an agent could otherwise confuse it with.
// Every input and output property needs its own description too, since that is
// where per-field meaning belongs rather than duplicated into the prose.
func TestToolDefinitionsDescribeThemselves(t *testing.T) {
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
			siblings, known := wantSiblings[tool.Name]
			if !known {
				t.Fatalf("tool %q has no routing requirement; add it to wantSiblings", tool.Name)
			}
			for _, sibling := range siblings {
				if !mentions(desc, sibling) {
					t.Errorf("description never mentions sibling %q, so an agent cannot route between them", sibling)
				}
			}
			assertPropertiesDescribed(t, "input", tool.InputSchema)
			assertPropertiesDescribed(t, "output", tool.OutputSchema)
		})
	}
}

// assertPropertiesDescribed requires every property of a tool's schema to carry
// a description. An undescribed field forces the tool description to explain it
// in prose instead, which is what the per-property descriptions exist to avoid.
func assertPropertiesDescribed(t *testing.T, which string, raw any) {
	t.Helper()
	if raw == nil {
		t.Errorf("%s schema is absent", which)
		return
	}
	schema, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s schema has unexpected type %T", which, raw)
	}
	// A schema with no properties at all is legitimate (list_models takes no
	// arguments), but a properties key of the wrong shape is not.
	rawProps, present := schema["properties"]
	if !present {
		return
	}
	props, ok := rawProps.(map[string]any)
	if !ok {
		t.Fatalf("%s schema properties have unexpected type %T", which, rawProps)
	}
	for name, rawProp := range props {
		prop, ok := rawProp.(map[string]any)
		if !ok {
			t.Errorf("%s property %q has unexpected type %T", which, name, rawProp)
			continue
		}
		if d, _ := prop["description"].(string); strings.TrimSpace(d) == "" {
			t.Errorf("%s property %q has no description", which, name)
		}
		// Recurse into an array's element schema. list_sessions returns its payload
		// as a list of objects, so without this the fields a client actually reads
		// (workspace, conversation_id) escape the requirement entirely.
		if items, ok := prop["items"].(map[string]any); ok {
			assertPropertiesDescribed(t, which+" "+name+" item", items)
		}
	}
}
