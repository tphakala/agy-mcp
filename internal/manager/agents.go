package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// listAgentsTimeout bounds `agy agents`, and listAgentsKillGrace bounds how long
// after that deadline the call waits before abandoning its output pipes. Same
// hazard as the version probe and the models listing: agy's descendants inherit
// those pipes, so one that outlives agy would otherwise hold the read open.
const (
	listAgentsTimeout   = 30 * time.Second
	listAgentsKillGrace = time.Second
)

// ListAgents lists agy's available agents: the names accepted as --agent. It
// decodes the JSON envelope from `agy --output-format json agents` rather than
// splitting the plain-text rows, so the names come from a typed field.
//
// An agent entry is a bare name, not the {id,label} pair a model is: agy prints
// agents as a single column and takes the name verbatim as --agent (measured
// against agy 1.1.24), so there is no id-vs-label reduction to make here (the
// distinction that list_models exists to preserve, issue #135). The return is a
// plain []string for that reason.
func (m *Manager) ListAgents(ctx context.Context) ([]string, error) {
	out, err := m.runJSONListing(ctx, "agents", listAgentsTimeout, listAgentsKillGrace)
	if err != nil {
		return nil, err
	}
	return decodeAgentsEnvelope(out)
}

const (
	// agentsEnvelopeStatusOK and agentsCommandName are the envelope framing
	// decodeAgentsEnvelope requires before it trusts the listing. They are the
	// literals agy prints, mirrored in internal/testutil/fakeagy.go.
	agentsEnvelopeStatusOK = "SUCCESS"
	agentsCommandName      = "agents"
)

// agentsEnvelope is the subset of agy's `--output-format json agents` output that
// list_agents needs: the status/command framing that proves the payload is the
// agents listing, and the name strings under command.data.agents. Fields agy also
// emits (a redundant `response` text column plus usage and timing metadata) are
// ignored.
//
// Agents is a POINTER so decodeAgentsEnvelope can tell an absent or null array
// (nil, which a `data`/`agents` rename or drop produces) from a present-but-empty
// one (a non-nil pointer to an empty slice, a legitimately empty catalog). A plain
// slice collapses both to nil and would read a renamed field as an empty catalog.
// This is the same guard modelsEnvelope carries; the payload differs only in that
// an agent is a bare string, not an {id,label} object.
type agentsEnvelope struct {
	Status  string `json:"status"`
	Command struct {
		Name string `json:"name"`
		Data struct {
			Agents *[]string `json:"agents"`
		} `json:"data"`
	} `json:"command"`
}

// decodeAgentsEnvelope turns agy's JSON agents envelope into the list of agent
// names, validating the framing before it trusts the list. It is an error, not a
// silently empty catalog, when the status is not SUCCESS, the command is not
// `agents`, or the command.data.agents array is absent or null: each is a shape
// agy-mcp does not recognize (a future field rename or restructure, or an error
// envelope that still exited 0), and cmd.Output() only catches a non-zero exit, so
// a well-formed-but-renamed envelope on exit 0 would otherwise decode to zero
// agents and mask the change. An envelope that IS the agents command and carries
// an explicit empty array is a legitimately empty catalog (no agents configured, a
// common state) and returns an empty, non-nil slice.
func decodeAgentsEnvelope(raw []byte) ([]string, error) {
	var env agentsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("agy agents: parse json envelope: %w", err)
	}
	if env.Status != agentsEnvelopeStatusOK {
		return nil, fmt.Errorf("agy agents: envelope status %q, want %q", env.Status, agentsEnvelopeStatusOK)
	}
	if env.Command.Name != agentsCommandName {
		return nil, fmt.Errorf("agy agents: envelope command %q, want %q", env.Command.Name, agentsCommandName)
	}
	// nil, not empty: an absent or null command.data.agents (a renamed or dropped
	// field), as opposed to a present "agents":[] which is a real empty catalog.
	if env.Command.Data.Agents == nil {
		return nil, errors.New("agy agents: envelope has no command.data.agents array")
	}
	names := *env.Command.Data.Agents
	agents := make([]string, 0, len(names))
	agents = append(agents, names...)
	return agents, nil
}
