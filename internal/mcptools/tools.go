// Package mcptools adapts the manager core to MCP tools.
package mcptools

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// runInput is the input for agy_run.
//
// The jsonschema tags are the per-property descriptions the client sees, so
// they carry the meaning that the Go types cannot: which fields are mutually
// exclusive, what a field defaults to, and what happens on expiry. Keeping that
// here rather than in the tool description avoids restating the schema in prose.
type runInput struct {
	Prompt         string   `json:"prompt" jsonschema:"the complete task for the delegated agent: what to do, the context needed to do it, and the output wanted back; the agent cannot see this conversation, so the prompt must stand alone"`
	Model          string   `json:"model,omitempty" jsonschema:"agy model name; omit to use the server's default model, or agy's own default when the server sets none. Call list_models for the accepted values"`
	Dirs           []string `json:"dirs,omitempty" jsonschema:"extra directories to grant the agent beyond cwd (agy --add-dir), e.g. a spec or a sibling repo. The agent can read and write there exactly as under cwd, so this widens the blast radius; prefer absolute paths"`
	ConversationID string   `json:"conversation_id,omitempty" jsonschema:"continue this specific conversation instead of starting fresh, so earlier context need not be restated; take it from a previous run's conversation_id or from list_sessions. Mutually exclusive with continue_latest"`
	ContinueLatest bool     `json:"continue_latest,omitempty" jsonschema:"continue the most recent conversation for cwd instead of starting fresh. Mutually exclusive with conversation_id: setting this true together with a conversation_id is an error, though leaving it false alongside one is fine"`
	Cwd            string   `json:"cwd,omitempty" jsonschema:"absolute path of the directory the agent runs in and may edit files under; also scopes continue_latest. Fresh runs sharing a cwd run concurrently. A relative path is resolved against the server's working directory, which is not necessarily yours, so pass an absolute one. Symlinks are resolved. Defaults to the server's own working directory"`
	Timeout        string   `json:"timeout,omitempty" jsonschema:"max wall-clock duration for the whole run (Go duration, e.g. 20m); a value over 24h is rejected. On expiry the agy process tree is killed and the job ends in state failed. Omit to use the server's default"`
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

// Tool annotations. ToolAnnotations models destructiveHint and openWorldHint as
// *bool because their spec default is true, so "unset" and "false" must stay
// distinguishable on the wire; new(true) / new(false) supply those pointers.
//
// Absent annotations are not neutral: the MCP spec defaults them to
// destructiveHint=true, openWorldHint=true, readOnlyHint=false, so a tool that
// ships none is advertising itself as a destructive open-world mutation. These
// declare the truth instead.
//
// destructiveHint and idempotentHint are meaningful only when readOnlyHint is
// false (see mcp.ToolAnnotations), so the read-only sets below deliberately
// leave them unset rather than carrying decorative values.
//
// Each var describes the PROPERTY it asserts, not the tools that currently use
// it: a list of tool names in a comment has nothing enforcing it and goes stale
// the moment a tool is added or reclassified. TestToolDefinitionsDeclareAnnotations
// pins the per-tool mapping instead, where a drift fails the build.
var (
	// annDelegate: spawns a full agy agent under --dangerously-skip-permissions,
	// so it can edit files under cwd and under every dir passed in, and may reach
	// the network. Each call starts a distinct job, so it is not idempotent.
	// destructiveHint is stated rather than left to default because the delegated
	// agent really can overwrite the repo; the SDK notes annotations are hints a
	// client need not honour, so this documents intent rather than enforcing it.
	annDelegate = &mcp.ToolAnnotations{
		DestructiveHint: new(true),
		OpenWorldHint:   new(true),
	}
	// annReadLocal: answers from local state (the job store, agy's conversation
	// cache) and changes nothing the caller can observe. Not literally
	// side-effect-free: reading a finished job's status memoizes the captured
	// conversation id into the job's meta (manager.persistCapturedID). That write
	// is idempotent bookkeeping over a value the job already produced, so
	// readOnlyHint still describes the blast radius accurately.
	annReadLocal = &mcp.ToolAnnotations{
		ReadOnlyHint:  true,
		OpenWorldHint: new(false),
	}
	// annReadExternal: read-only like annReadLocal, but answers by shelling out to
	// the agy CLI, whose model registry lives outside this server.
	annReadExternal = &mcp.ToolAnnotations{
		ReadOnlyHint:  true,
		OpenWorldHint: new(true),
	}
	// annCancel: terminates work in progress, which is destructive, but
	// re-cancelling an already-cancelled job has no further effect. idempotentHint
	// is a third field whose default this flips (false to true); some clients read
	// it as licence to retry the call after a transport failure. Retrying re-sends
	// the cancel request, which the supervisor's buffered signal channel absorbs
	// while it is still listening.
	annCancel = &mcp.ToolAnnotations{
		DestructiveHint: new(true),
		IdempotentHint:  true,
		OpenWorldHint:   new(false),
	}
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
	JobID          string `json:"job_id" jsonschema:"handle for this run; pass it to agy_wait, agy_status or agy_cancel"`
	ConversationID string `json:"conversation_id,omitempty" jsonschema:"conversation this run belongs to; pass it back as conversation_id to continue the thread. Rarely empty on a fresh run, when agy had not yet named the conversation; agy_status reports it moments later"`
	State          string `json:"state" jsonschema:"always running: the job has been started and cannot have finished yet. Block with agy_wait or check agy_status for the outcome"`
}

type statusInput struct {
	JobID string `json:"job_id" jsonschema:"job id returned by agy_run or agy_run_sync"`
}

type statusOutput struct {
	State          string `json:"state" jsonschema:"running, done, failed or cancelled"`
	Elapsed        string `json:"elapsed" jsonschema:"wall-clock time the job has run, frozen at completion once terminal"`
	Result         string `json:"result,omitempty" jsonschema:"the delegated agent's output. Set whenever the run produced any text, so a failed or cancelled job can carry one too; read it in those states rather than discarding it, but check partial first"`
	Error          string `json:"error,omitempty" jsonschema:"why the job failed; present only when state is failed"`
	ConversationID string `json:"conversation_id,omitempty" jsonschema:"conversation this run belongs to; pass it back as conversation_id to continue the thread"`
	// Partial marks a result that is not a verified final answer; see
	// manager.Status.Partial.
	Partial bool `json:"partial,omitempty" jsonschema:"true when result is not the verified final answer, so treat it as incomplete. It follows from where the text came from: true when the text was reconstructed from the streamed events, because agy never reported a terminal result or the one it reported carried no text, and true when agy reported a terminal result that was not a success, so its text is only what the run had produced when it stopped. A response agy itself marked successful is never partial, even on a job that was then cancelled or killed"`
	// NumTurns and Usage are agy's own accounting, present once the run reported
	// a terminal result.
	NumTurns int          `json:"num_turns,omitempty" jsonschema:"how many turns the conversation has taken, as reported by agy"`
	Usage    *usageOutput `json:"usage,omitempty" jsonschema:"token accounting for the run, as reported by agy"`
	// StepType answers "what is it doing" for a running job, which is the whole
	// point of polling one. It reaches the wire as well as the progress
	// notification because Claude Code never surfaces notifications to the model.
	StepType string `json:"step_type,omitempty" jsonschema:"what agy is doing right now (for example agent_response or a tool call), for a running job; a hint that lags by up to one poll"`
}

// usageOutput mirrors agy's usage object on the wire. It is declared here rather
// than reusing the streamjson type so the tool schema owns its own field
// documentation.
type usageOutput struct {
	InputTokens     int `json:"input_tokens" jsonschema:"prompt tokens billed for this run"`
	OutputTokens    int `json:"output_tokens" jsonschema:"tokens generated, including thinking"`
	ThinkingTokens  int `json:"thinking_tokens" jsonschema:"the share of output tokens spent on reasoning"`
	CacheReadTokens int `json:"cache_read_tokens" jsonschema:"prompt tokens served from the provider's cache"`
	TotalTokens     int `json:"total_tokens" jsonschema:"input plus output tokens"`
}

// toStatusOutput converts a manager status into its wire shape, shared by
// agy_status, agy_run_sync and agy_wait so they cannot drift.
func toStatusOutput(st manager.Status) statusOutput {
	out := statusOutput{
		State:          st.State,
		Elapsed:        st.Elapsed.Round(time.Second).String(),
		Result:         st.Result,
		Error:          st.Error,
		ConversationID: st.ConversationID,
		Partial:        st.Partial,
		NumTurns:       st.NumTurns,
		StepType:       st.StepType,
	}
	if u := st.Usage; u != nil {
		out.Usage = &usageOutput{
			InputTokens:     u.InputTokens,
			OutputTokens:    u.OutputTokens,
			ThinkingTokens:  u.ThinkingTokens,
			CacheReadTokens: u.CacheReadTokens,
			TotalTokens:     u.TotalTokens,
		}
	}
	return out
}

type cancelInput struct {
	JobID string `json:"job_id" jsonschema:"job id to cancel, as returned by agy_run or agy_run_sync"`
}
type cancelOutput struct {
	State string `json:"state" jsonschema:"state right after the cancel was requested: usually still running, since termination is asynchronous and settles to cancelled shortly after; a terminal state if the job had already finished, or unknown if the state could not be read"`
}

type emptyInput struct{}

type modelsOutput struct {
	Models []string `json:"models" jsonschema:"model names accepted by the model parameter of agy_run and agy_run_sync"`
}

type sessionsInput struct {
	Dir string `json:"dir,omitempty" jsonschema:"absolute path of a workspace directory to filter to; omit to list every known conversation. The filter is canonicalized (made absolute against the server's working directory, symlinks resolved) before matching, so a path agy cached in some other spelling may not match"`
}
type sessionsOutput struct {
	Sessions []manager.Session `json:"sessions" jsonschema:"one entry per workspace directory, each holding that directory's most recent conversation id"`
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
- agy_run_sync starts a run and waits inline (bounded by the wait argument). Use it when you need the answer before your next step and the task is bounded enough to finish within that wait.
- agy_run returns a job_id in under a couple of seconds (it waits briefly for agy to name the conversation); block on it with agy_wait or poll it with agy_status. Use these for long runs, for open-ended work that routinely outlives an inline wait (web research, a large review), or to fan several tasks out in parallel.
- Outliving the inline wait is not a failure: the job keeps running under its own supervisor and the returned job_id still resolves to its outcome. Wait on it or poll it; do not re-send the prompt.
- list_models enumerates models; call it only if you want to override the default. list_sessions lists known conversations.

Notes:
- Continue a prior thread with conversation_id or continue_latest instead of restating context.
- Fan out freely: fresh runs in the same directory run concurrently, up to the server's concurrency cap. Only two runs continuing the SAME conversation_id conflict, and the second is refused rather than queued.
- agy runs a full agent that can edit files. For a review that must not touch the repo, say so explicitly in the prompt.
- Always reconcile a backgrounded run: poll it to completion and fold the result back in.`

// NewServer builds an MCP server with all agy tools registered.
func NewServer(mgr *manager.Manager) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agy-mcp", Version: serverVersion()}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolAgyRun,
		Title:       "Delegate to agy (async)",
		Annotations: annDelegate,
		Description: "Delegate a prompt to a background agy model as an async job (peer review, research, or any self-contained task) and keep working. Returns a job_id; block on it with agy_wait or poll it with agy_status. Prefer this over agy_run_sync for long runs, for open-ended work that routinely outlives an inline wait (web research, a large review), and to fan several tasks out in parallel; use agy_run_sync instead when you need the answer before your next step. The delegated agent runs with permission checks disabled: it can edit files under cwd and under any dirs, and may reach the network. Say so in the prompt if the run must not touch the repo.",
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
		Title:       "Check an agy job",
		Annotations: annReadLocal,
		Description: "Check an agy job once, without blocking. Use it for a single snapshot, or to check several jobs in turn; when the next step depends on one job's result, prefer a single agy_wait over a poll loop. Checking a still-running job is cheap, but every check on a finished one re-reads its full output, so collect a result once rather than repeatedly.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in statusInput) (*mcp.CallToolResult, statusOutput, error) {
		st, err := mgr.Status(in.JobID)
		if err != nil {
			return nil, statusOutput{}, err
		}
		return nil, toStatusOutput(st), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        toolAgyCancel,
		Title:       "Cancel an agy job",
		Annotations: annCancel,
		Description: "Stop a running agy job: asks its supervisor to terminate the agy process tree. Use it to abandon a job whose result is no longer needed, or one that is stuck; there is no resume, so continuing the work means a new agy_run. Before re-sending the prompt, read the cancelled job with agy_status: a cancelled run still carries whatever text it produced, and a run agy had already finished carries a complete, non-partial answer, so re-running it would pay for the same work twice. Termination is asynchronous, so the returned state is usually still running and settles to cancelled a moment later; that is a delivered cancel, not a failed one. Calling it on an already-finished job is a harmless no-op. Files the delegated agent already wrote are not rolled back.",
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
		Name:        toolListModels,
		Title:       "List agy models",
		Annotations: annReadExternal,
		Description: "List the agy model names accepted by the model parameter of agy_run and agy_run_sync. Call it only when you intend to override the default; omitting model picks a default already, so most runs need no call here. Shells out to the agy CLI, so it fails if agy is missing from PATH or not authenticated.",
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
		Name:        toolListSessions,
		Title:       "List agy conversations",
		Annotations: annReadLocal,
		Description: "List known agy conversations as workspace-directory to conversation-id pairs. Use it to recover a conversation_id to pass to agy_run or agy_run_sync when resuming an earlier thread, which beats restating context in a fresh prompt. To continue the latest thread for a directory you do not need this: set continue_latest instead. Reads agy's own conversation cache, so it also lists conversations this server never started, and it holds only the most recent id per workspace rather than full history.",
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
