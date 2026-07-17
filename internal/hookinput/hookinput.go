// Package hookinput extracts the agy job id from a Claude Code PostToolUse
// hook payload, for the hook-wait subcommand. Parsing is deliberately loose:
// the payload's tool_response shape depends on the MCP client version (plain
// structured output, or a content list whose text embeds the output JSON), so
// the job id is found by walking, not by a fixed schema.
package hookinput

import (
	"encoding/json"
	"io"
	"strings"
)

// maxDepth bounds the recursive walk so a pathological payload cannot
// exhaust the stack. Real payloads are a handful of levels deep.
const maxDepth = 32

// payload is the subset of the PostToolUse hook input hook-wait needs.
type payload struct {
	ToolName     string          `json:"tool_name"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

// Parse decodes a PostToolUse payload from r and extracts the agy job id from
// its tool response. ok is false when no job id is present; that is not an
// error, a failed or foreign tool call simply has no job to wait for.
func Parse(r io.Reader) (jobID, toolName string, ok bool) {
	var p payload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return "", "", false
	}
	var resp any
	if err := json.Unmarshal(p.ToolResponse, &resp); err != nil {
		return "", p.ToolName, false
	}
	id := findJobID(resp, 0)
	return id, p.ToolName, id != ""
}

// findJobID walks maps and slices for a "job_id" string field. String values
// that look like they embed JSON with a job id (MCP text content) are parsed
// and walked too, so the id is found regardless of which layer carries it.
func findJobID(v any, depth int) string {
	if depth > maxDepth {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		if id, isStr := t["job_id"].(string); isStr && id != "" {
			return id
		}
		for _, child := range t {
			if id := findJobID(child, depth+1); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range t {
			if id := findJobID(child, depth+1); id != "" {
				return id
			}
		}
	case string:
		if strings.Contains(t, `"job_id"`) {
			var embedded any
			if err := json.Unmarshal([]byte(t), &embedded); err == nil {
				return findJobID(embedded, depth+1)
			}
		}
	}
	return ""
}
