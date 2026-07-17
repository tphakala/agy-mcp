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

// maxNodes bounds the total nodes findField visits per field extraction, so a
// payload that is shallow but very wide, or that nests re-parseable JSON strings,
// cannot make the walk do unbounded work within the depth limit. Real payloads
// visit a handful of nodes.
const maxNodes = 1 << 20

// maxPayloadBytes caps how much of stdin Parse decodes. It is generous against
// legitimate agy outputs (tens of MiB of captured review text is plausible) yet
// stops an unbounded read from a stuck or hostile producer. A truncated decode
// fails and Parse returns ok=false, so the hook simply stays quiet; the cap must
// keep real margin over real payloads for that quiet-on-failure to be safe.
const maxPayloadBytes = 64 << 20

// payload is the subset of the PostToolUse hook input hook-wait needs.
type payload struct {
	ToolName     string          `json:"tool_name"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

// Parse decodes a PostToolUse payload from r and extracts the agy job id and
// state from its tool response. ok is false when no job id is present; that
// is not an error, a failed or foreign tool call simply has no job to wait
// for. state is the response's own job-state string when present (e.g.
// "running", "done"), or empty; it lets a caller tell a result that was
// still running when the tool returned (an overrun) apart from one that had
// already settled.
func Parse(r io.Reader) (jobID, toolName, state string, ok bool) {
	var p payload
	// Cap the decode so a stuck or hostile producer cannot make the hook read
	// without bound; a truncated read fails here and the hook stays quiet.
	if err := json.NewDecoder(io.LimitReader(r, maxPayloadBytes)).Decode(&p); err != nil {
		return "", "", "", false
	}
	var resp any
	if err := json.Unmarshal(p.ToolResponse, &resp); err != nil {
		return "", p.ToolName, "", false
	}
	id := extractField(resp, "job_id")
	st := extractField(resp, "state")
	return id, p.ToolName, st, id != ""
}

// extractField finds a named string field in a tool response, checking the
// well-known MCP result keys in a fixed priority order before falling back
// to an unordered walk of the whole response. A response can carry both an
// authoritative structuredContent object and a content list whose text is
// free-form model output; that free-form text might itself contain
// something that looks like the field being searched for (an adversarial
// payload, or an unlucky coincidence), so structuredContent is checked, and
// wins, before content is ever consulted. The final fallback walk covers
// every other shape, including the field sitting directly on the response
// (as it does today from the MCP SDK's own tool results).
func extractField(resp any, key string) string {
	// The marker is the field's quoted JSON name, derived from key so the two
	// cannot drift; findField uses it to spot the field embedded in a JSON string.
	marker := `"` + key + `"`
	// One work budget per extraction, shared across the priority sources below, so
	// breadth or re-parse repetition cannot make the walk do unbounded work.
	budget := maxNodes
	if m, isMap := resp.(map[string]any); isMap {
		if sc, has := m["structuredContent"]; has {
			if v := findField(sc, key, marker, &budget, 0); v != "" {
				return v
			}
		}
		if content, has := m["content"]; has {
			if v := findField(content, key, marker, &budget, 0); v != "" {
				return v
			}
		}
	}
	return findField(resp, key, marker, &budget, 0)
}

// findField walks maps and slices for a string field named key. String values
// that look like they embed JSON with that field (detected via marker, the
// field's quoted JSON name) are parsed and walked too, so the field is found
// regardless of which layer carries it. budget is decremented once per node
// visited and the walk bails when it reaches zero, so a wide or re-parse-heavy
// payload cannot defeat the depth limit by doing unbounded work at shallow depth.
func findField(v any, key, marker string, budget *int, depth int) string {
	if depth > maxDepth || *budget <= 0 {
		return ""
	}
	*budget--
	switch t := v.(type) {
	case map[string]any:
		// A present key wins for this object even when its value is the empty
		// string: present-but-empty reads as absent upstream (the safe direction),
		// and this object's own field must not be overridden by one nested deeper,
		// so stop here rather than scavenge this object's other keys.
		if val, isStr := t[key].(string); isStr {
			return val
		}
		for _, child := range t {
			if val := findField(child, key, marker, budget, depth+1); val != "" {
				return val
			}
		}
	case []any:
		for _, child := range t {
			if val := findField(child, key, marker, budget, depth+1); val != "" {
				return val
			}
		}
	case string:
		if strings.Contains(t, marker) {
			var embedded any
			if err := json.Unmarshal([]byte(t), &embedded); err == nil {
				return findField(embedded, key, marker, budget, depth+1)
			}
		}
	}
	return ""
}
