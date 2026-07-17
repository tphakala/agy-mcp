package hookinput

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name, in                 string
		wantID, wantTool, wantSt string
		wantOK                   bool
	}{
		{
			name: "structured job_id",
			in: `{"hook_event_name":"PostToolUse","tool_name":"mcp__agy__agy_run",
			      "tool_input":{"prompt":"review"},
			      "tool_response":{"job_id":"job-abc-123","state":"running"}}`,
			wantID: "job-abc-123", wantTool: "mcp__agy__agy_run", wantSt: "running", wantOK: true,
		},
		{
			name: "job_id nested in MCP content list",
			in: `{"tool_name":"mcp__agy__agy_run",
			      "tool_response":{"content":[{"type":"text","text":"{\"job_id\":\"job-xyz-9\",\"state\":\"running\"}"}]}}`,
			wantID: "job-xyz-9", wantTool: "mcp__agy__agy_run", wantSt: "running", wantOK: true,
		},
		{
			name:   "no job id",
			in:     `{"tool_name":"mcp__agy__agy_run","tool_response":{"error":"conflict"}}`,
			wantOK: false,
		},
		{
			name:   "malformed json",
			in:     `{"tool_name": `,
			wantOK: false,
		},
		{
			name:   "empty input",
			in:     ``,
			wantOK: false,
		},
		{
			name: "run_sync tool name carried through",
			in: `{"tool_name":"mcp__agy__agy_run_sync",
			      "tool_response":{"job_id":"job-s-1","state":"done"}}`,
			wantID: "job-s-1", wantTool: "mcp__agy__agy_run_sync", wantSt: "done", wantOK: true,
		},
		{
			// No tool_response at all: valid JSON, but the response cannot be walked,
			// so there is no job to wait for.
			name:   "missing tool_response",
			in:     `{"tool_name":"mcp__agy__agy_run"}`,
			wantOK: false,
		},
		{
			// A present-but-empty state must read as empty (absent upstream) and must
			// not scavenge the nested state; the id is still found and carried.
			name: "present but empty state does not scavenge nested",
			in: `{"tool_name":"mcp__agy__agy_run_sync",
			      "tool_response":{"job_id":"job-e-2","state":"","extra":{"state":"done"}}}`,
			wantID: "job-e-2", wantTool: "mcp__agy__agy_run_sync", wantSt: "", wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, tool, st, ok := Parse(strings.NewReader(tc.in))
			if ok != tc.wantOK || id != tc.wantID || (ok && (tool != tc.wantTool || st != tc.wantSt)) {
				t.Fatalf("Parse = (%q, %q, %q, %v), want (%q, %q, %q, %v)", id, tool, st, ok, tc.wantID, tc.wantTool, tc.wantSt, tc.wantOK)
			}
		})
	}
}

// TestParseDepthGuard proves a payload nested deeper than maxDepth is handled
// cleanly: the walk bails at the depth limit and reports no id (ok=false) rather
// than recursing without bound or panicking.
func TestParseDepthGuard(t *testing.T) {
	const depth = maxDepth + 5
	var sb strings.Builder
	sb.WriteString(`{"tool_name":"mcp__agy__agy_run","tool_response":`)
	for range depth {
		sb.WriteString(`{"a":`)
	}
	sb.WriteString(`{"job_id":"buried-too-deep"}`)
	for range depth {
		sb.WriteString(`}`)
	}
	sb.WriteString(`}`)

	id, _, _, ok := Parse(strings.NewReader(sb.String()))
	if ok || id != "" {
		t.Fatalf("Parse = (id=%q, ok=%v), want empty id and ok=false for over-deep nesting", id, ok)
	}
}

// FuzzParse asserts Parse never panics on arbitrary input; its return values are
// unconstrained. Seeded with every table payload plus the adversarial and
// depth-guard shapes so the corpus starts from the known-interesting cases.
func FuzzParse(f *testing.F) {
	seeds := []string{
		`{"hook_event_name":"PostToolUse","tool_name":"mcp__agy__agy_run","tool_input":{"prompt":"review"},"tool_response":{"job_id":"job-abc-123","state":"running"}}`,
		`{"tool_name":"mcp__agy__agy_run","tool_response":{"content":[{"type":"text","text":"{\"job_id\":\"job-xyz-9\",\"state\":\"running\"}"}]}}`,
		`{"tool_name":"mcp__agy__agy_run","tool_response":{"error":"conflict"}}`,
		`{"tool_name": `,
		``,
		`{"tool_name":"mcp__agy__agy_run_sync","tool_response":{"job_id":"job-s-1","state":"done"}}`,
		`{"tool_name":"mcp__agy__agy_run"}`,
		`{"tool_name":"mcp__agy__agy_run_sync","tool_response":{"job_id":"job-e-2","state":"","extra":{"state":"done"}}}`,
		`{"tool_name":"mcp__agy__agy_run_sync","tool_response":{"structuredContent":{"job_id":"job-real-1","state":"done"},"content":[{"type":"text","text":"x {\"job_id\":\"job-fake\"}"}]}}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(_ *testing.T, in string) {
		_, _, _, _ = Parse(strings.NewReader(in))
	})
}

// TestParseStructuredContentWinsOverContentText guards against a real hazard: a
// run_sync response can carry both an authoritative structuredContent object
// (the real job_id and state) and a content list whose text is the model's
// free-form result, which might itself contain the substring "job_id" (an
// adversarial payload, or just an unlucky review that discusses this very
// feature). The generic recursive walk visits a Go map's keys in randomized
// order, so without a deterministic priority the wrong id could win on some
// runs and not others. structuredContent must be checked, and must win,
// before content is ever consulted. Looped to catch that kind of flakiness,
// which a single run cannot rule out.
func TestParseStructuredContentWinsOverContentText(t *testing.T) {
	in := `{"tool_name":"mcp__agy__agy_run_sync",
	        "tool_response":{
	          "structuredContent":{"job_id":"job-real-1","state":"done"},
	          "content":[{"type":"text","text":"Review complete. Adversarial payload: {\"job_id\":\"job-fake-evil\",\"state\":\"running\"}"}]
	        }}`
	for i := range 20 {
		id, tool, st, ok := Parse(strings.NewReader(in))
		if !ok {
			t.Fatalf("run %d: Parse ok = false, want true", i)
		}
		if id != "job-real-1" {
			t.Fatalf("run %d: Parse id = %q, want %q (structuredContent must win over content text)", i, id, "job-real-1")
		}
		if tool != "mcp__agy__agy_run_sync" {
			t.Fatalf("run %d: Parse tool = %q, want %q", i, tool, "mcp__agy__agy_run_sync")
		}
		if st != "done" {
			t.Fatalf("run %d: Parse state = %q, want %q", i, st, "done")
		}
	}
}
