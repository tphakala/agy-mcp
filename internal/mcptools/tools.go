// Package mcptools adapts the manager core to MCP tools.
package mcptools

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
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

// maxJobTimeout caps a client-supplied per-job timeout. It bounds both the agy
// --print-timeout and the supervisor's hard-kill deadline, so a typo like "1000h"
// cannot leave a hung job uncollectable for weeks.
const maxJobTimeout = 24 * time.Hour

// ToolAgyRunSync is the agy_run_sync tool name, exported so out-of-package
// callers (the hook-wait suppression gate) can match it against a hook payload's
// recorded tool name with a compile-time link instead of a duplicated literal.
const ToolAgyRunSync = "agy_run_sync"

// Tool names, shared between registration (here and run_sync.go) and tests so
// the two cannot drift.
const (
	toolAgyRun       = "agy_run"
	toolAgyStatus    = "agy_status"
	toolAgyCancel    = "agy_cancel"
	toolAgyRunSync   = ToolAgyRunSync
	toolAgyWait      = "agy_wait"
	toolListModels   = "list_models"
	toolListSessions = "list_sessions"
)

// toStartRequest converts the wire input into a manager start request,
// validating the timeout.
func (in runInput) toStartRequest() (manager.StartRequest, error) {
	req := manager.StartRequest{
		Prompt: in.Prompt, Model: in.Model, Dirs: in.Dirs,
		ConversationID: in.ConversationID, ContinueLatest: in.ContinueLatest, Cwd: in.Cwd,
	}
	if in.Timeout != "" {
		d, err := time.ParseDuration(in.Timeout)
		if err != nil || d <= 0 {
			return manager.StartRequest{}, fmt.Errorf("invalid timeout %q: want a positive Go duration like 20m", in.Timeout)
		}
		if d > maxJobTimeout {
			return manager.StartRequest{}, fmt.Errorf("timeout %q exceeds the maximum of %s", in.Timeout, maxJobTimeout)
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
	// Partial is set when a done job's result was recovered without a completion
	// sentinel and so may be truncated; see manager.Status.Partial.
	Partial bool `json:"partial,omitempty"`
}

// toStatusOutput converts a manager status into its wire shape, shared by
// agy_status and agy_run_sync so the two cannot drift.
func toStatusOutput(st manager.Status) statusOutput {
	return statusOutput{
		State:          st.State,
		Elapsed:        st.Elapsed.Round(time.Second).String(),
		Result:         st.Result,
		Error:          st.Error,
		ConversationID: st.ConversationID,
		Partial:        st.Partial,
	}
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

// serverVersion reports the module version the Go toolchain stamped into the
// binary (e.g. v1.0.0 for a tagged `go install`, a pseudo-version for builds
// past a tag), or "dev" when no version was stamped (plain `go test` binaries).
// It is memoized with sync.OnceValue: the build info is fixed for a process's
// life, but NewServer (and thus this) is called once per HTTP session.
var serverVersion = sync.OnceValue(func() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
})

// serverInstructions is the MCP instructions string sent to clients at
// initialize (injected into the client's system prompt). It is neutral
// capability documentation: what agy is for and when a client should reach for
// these tools. It deliberately encodes no single client's conventions or policy
// (model choice, working-directory habits, review etiquette); those belong in
// that client's own configuration, not in a server shared by everyone.
const serverInstructions = `agy delegates a prompt to a background coding agent and manages the run as a restart-resilient job with output captured to disk. Use it to get an independent perspective or to offload self-contained work while you keep going:

- Peer review: have another model review code, a design, or a plan for bugs, security, and correctness.
- Rubber-duck: talk through a decision and get pushback that catches blind spots your own reasoning would skip.
- Research: fact-check a claim or look into a topic.
- Background delegation: fire off an independent task and reconcile the result later, so two things run at once.

Choosing a tool:
- agy_run_sync starts a run and waits inline (bounded by the wait argument). Use it when you need the answer before your next step.
- agy_run returns a job_id immediately; poll it with agy_status or block on it with agy_wait. Use these for long runs or to fan several tasks out in parallel.
- list_models enumerates models; call it only if you want to override the default. list_sessions lists known conversations.

Notes:
- Continue a prior thread with conversation_id or continue_latest instead of restating context.
- Fresh runs sharing a cwd are not queued; a concurrent attempt returns a conflict error. To run tasks in parallel, continue an existing conversation or use a different directory.
- agy runs a full agent that can edit files. For a review that must not touch the repo, say so explicitly in the prompt.
- Always reconcile a backgrounded run: poll it to completion and fold the result back in.`

// NewServer builds an MCP server with all agy tools registered.
func NewServer(mgr *manager.Manager) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agy-mcp", Version: serverVersion()}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolAgyRun,
		Description: "Delegate a prompt to a background agy model as an async job (peer review, research, or any self-contained task) and keep working. Returns a job_id; poll with agy_status or block with agy_wait. Prefer this over agy_run_sync for long runs or to fan several tasks out in parallel.",
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
		Name:        toolAgyStatus,
		Description: `Poll an agy job. Returns running, done (with result), failed, or cancelled. A done result with "partial": true was recovered without a completion sentinel and may be truncated.`,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in statusInput) (*mcp.CallToolResult, statusOutput, error) {
		st, err := mgr.Status(in.JobID)
		if err != nil {
			return nil, statusOutput{}, err
		}
		return nil, toStatusOutput(st), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolAgyCancel,
		Description: `Cancel a running agy job. Returns the resulting state: "cancelled", or the job's terminal state if it had already finished, or "unknown" if the state could not be read after cancelling.`,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cancelInput) (*mcp.CallToolResult, cancelOutput, error) {
		if err := mgr.Cancel(in.JobID); err != nil {
			return nil, cancelOutput{}, err
		}
		// Cancel itself succeeded; report the resulting state, or "unknown" if the
		// job state is no longer readable. State (not Status) is used here: the
		// follow-up only needs the state, and reading the full result would mean
		// loading a potentially large out file just to discard it.
		state := "unknown"
		if s, err := mgr.State(in.JobID); err == nil {
			state = s
		}
		return nil, cancelOutput{State: state}, nil
	})

	registerRunSync(s, mgr)
	registerWait(s, mgr)

	mcp.AddTool(s, &mcp.Tool{
		Name: toolListModels, Description: "List available agy models. Call this to see the options if you want to override the default model for agy_run or agy_run_sync.",
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
		Name: toolListSessions, Description: "List known agy conversations (workspace to conversation id).",
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
