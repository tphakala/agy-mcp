// Package mcptools adapts the manager core to MCP tools.
package mcptools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// runInput is the input for agy_run.
type runInput struct {
	Prompt         string   `json:"prompt" jsonschema:"the prompt to send to agy"`
	Model          string   `json:"model,omitempty" jsonschema:"agy model name; defaults to agy's configured default"`
	Dirs           []string `json:"dirs,omitempty" jsonschema:"extra workspace directories (--add-dir)"`
	ConversationID string   `json:"conversation_id,omitempty" jsonschema:"continue a specific conversation"`
	ContinueLatest bool     `json:"continue_latest,omitempty" jsonschema:"continue the most recent conversation for cwd"`
	Cwd            string   `json:"cwd,omitempty" jsonschema:"working directory for the run"`
	Timeout        string   `json:"timeout,omitempty" jsonschema:"max run duration, e.g. 20m"`
}

// toStartRequest converts the wire input into a manager start request,
// validating the timeout.
func (in runInput) toStartRequest() (manager.StartRequest, error) {
	req := manager.StartRequest{
		Prompt: in.Prompt, Model: in.Model, Dirs: in.Dirs,
		ConversationID: in.ConversationID, ContinueLatest: in.ContinueLatest, Cwd: in.Cwd,
	}
	if in.Timeout != "" {
		d, err := time.ParseDuration(in.Timeout)
		if err != nil {
			return manager.StartRequest{}, fmt.Errorf("invalid timeout %q: %w", in.Timeout, err)
		}
		req.Timeout = d
	}
	return req, nil
}

type runOutput struct {
	JobID          string `json:"job_id"`
	ConversationID string `json:"conversation_id,omitempty"`
	State          string `json:"state"`
}

type statusInput struct {
	JobID string `json:"job_id" jsonschema:"the job id returned by agy_run"`
}

type statusOutput struct {
	State          string `json:"state"`
	Elapsed        string `json:"elapsed"`
	Result         string `json:"result,omitempty"`
	Error          string `json:"error,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
}

type cancelInput struct {
	JobID string `json:"job_id" jsonschema:"the job id to cancel"`
}
type cancelOutput struct {
	State string `json:"state"`
}

type emptyInput struct{}

type modelsOutput struct {
	Models []string `json:"models"`
}

type sessionsInput struct {
	Dir string `json:"dir,omitempty" jsonschema:"filter to one workspace directory"`
}
type sessionsOutput struct {
	Sessions []manager.Session `json:"sessions"`
}

// NewServer builds an MCP server with all agy tools registered.
func NewServer(mgr *manager.Manager) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agy-mcp", Version: "0.1.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "agy_run",
		Description: "Start an agy prompt (e.g. a peer review) as an async job. Returns a job_id to poll with agy_status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, runOutput, error) {
		req, err := in.toStartRequest()
		if err != nil {
			return nil, runOutput{}, err
		}
		job, err := mgr.StartJob(req)
		if err != nil {
			return nil, runOutput{}, err
		}
		return nil, runOutput{JobID: job.ID, ConversationID: job.ConversationID, State: job.State}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "agy_status",
		Description: "Poll an agy job. Returns running, done (with result), failed, or cancelled.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in statusInput) (*mcp.CallToolResult, statusOutput, error) {
		st, err := mgr.Status(in.JobID)
		if err != nil {
			return nil, statusOutput{}, err
		}
		return nil, statusOutput{
			State: st.State, Elapsed: st.Elapsed.Round(1e9).String(),
			Result: st.Result, Error: st.Error, ConversationID: st.ConversationID,
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "agy_cancel", Description: "Cancel a running agy job.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cancelInput) (*mcp.CallToolResult, cancelOutput, error) {
		if err := mgr.Cancel(in.JobID); err != nil {
			return nil, cancelOutput{}, err
		}
		// Cancel itself succeeded; report the resulting state, or "unknown" if the
		// job state is no longer readable.
		state := "unknown"
		if st, err := mgr.Status(in.JobID); err == nil {
			state = st.State
		}
		return nil, cancelOutput{State: state}, nil
	})

	registerRunSync(s, mgr)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_models", Description: "List available agy models.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, modelsOutput, error) {
		models, err := mgr.ListModels(ctx)
		if err != nil {
			return nil, modelsOutput{}, err
		}
		if models == nil {
			models = []string{}
		}
		return nil, modelsOutput{Models: models}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_sessions", Description: "List known agy conversations (workspace to conversation id).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sessionsInput) (*mcp.CallToolResult, sessionsOutput, error) {
		sessions, err := mgr.ListSessions(in.Dir)
		if err != nil {
			return nil, sessionsOutput{}, err
		}
		if sessions == nil {
			sessions = []manager.Session{}
		}
		return nil, sessionsOutput{Sessions: sessions}, nil
	})

	return s
}
