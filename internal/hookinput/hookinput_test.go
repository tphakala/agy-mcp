package hookinput

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name, in         string
		wantID, wantTool string
		wantOK           bool
	}{
		{
			name: "structured job_id",
			in: `{"hook_event_name":"PostToolUse","tool_name":"mcp__agy__agy_run",
			      "tool_input":{"prompt":"review"},
			      "tool_response":{"job_id":"job-abc-123","state":"running"}}`,
			wantID: "job-abc-123", wantTool: "mcp__agy__agy_run", wantOK: true,
		},
		{
			name: "job_id nested in MCP content list",
			in: `{"tool_name":"mcp__agy__agy_run",
			      "tool_response":{"content":[{"type":"text","text":"{\"job_id\":\"job-xyz-9\",\"state\":\"running\"}"}]}}`,
			wantID: "job-xyz-9", wantTool: "mcp__agy__agy_run", wantOK: true,
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
			wantID: "job-s-1", wantTool: "mcp__agy__agy_run_sync", wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, tool, ok := Parse(strings.NewReader(tc.in))
			if ok != tc.wantOK || id != tc.wantID || (ok && tool != tc.wantTool) {
				t.Fatalf("Parse = (%q, %q, %v), want (%q, %q, %v)", id, tool, ok, tc.wantID, tc.wantTool, tc.wantOK)
			}
		})
	}
}
