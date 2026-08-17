// Package testutil provides test doubles for agy-mcp.
package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

// defaultFakeConversationID is the conversation id a FakeAgy reports when the
// test does not pin one. It is a well-formed UUID so it reads like a real agy
// value in failure output. Tests read it through ConvID rather than directly,
// which is why it is not exported.
const defaultFakeConversationID = "11111111-2222-3333-4444-555555555555"

// FakeAgy configures a stand-in agy binary for tests.
//
// The generated script reproduces agy's `--output-format stream-json` print
// mode: an init event naming the conversation, one agent_response step carrying
// the text, and a terminal result event. The event stream is marshalled in Go at
// setup time and replayed with cat, so arbitrary response text survives without
// hand-rolling JSON escaping in shell.
type FakeAgy struct {
	// Stdout is the response text. It is emitted as the agent_response step's
	// text_delta and as the terminal result's response, mirroring what real agy
	// does for a single-turn run.
	Stdout string
	Stderr string        // printed to stderr
	Exit   int           // exit code
	Sleep  time.Duration // delay before printing (simulate a long run); bash sleep honors fractions
	// IgnoreSIGTERM makes the script trap and ignore SIGTERM and loop forever, so
	// only SIGKILL can stop it. Used to exercise the supervisor's SIGKILL
	// escalation after killGrace. Mutually exclusive with the output fields (the
	// process never reaches them).
	IgnoreSIGTERM bool

	// ConversationID overrides the conversation id in the emitted events.
	// Defaults to defaultFakeConversationID.
	ConversationID string
	// NoConversationID emits events with no conversation id, reproducing a run
	// that failed before agy created a conversation (an unresolvable model, say).
	// It overrides ConversationID.
	NoConversationID bool
	// Status is the terminal result's status. Defaults to SUCCESS.
	Status string
	// ResultError populates the terminal result's error field, for an ERROR
	// status.
	ResultError string
	// OmitResult emits the init and step events but no terminal result, standing
	// in for a run cut short before agy could summarize it.
	OmitResult bool

	// Version is what the script reports for `agy --version`, which the manager
	// probes once before it will run anything. Defaults to the minimum agy-mcp
	// accepts; set it lower to exercise the refusal.
	Version string

	// Models is the catalog the fake reports for `agy --output-format json models`,
	// one {id,label} object per entry in the envelope's command.data.models array.
	// An empty Models yields a well-formed envelope with an empty (non-null) models
	// array, so the empty-catalog path can be exercised. It is independent of
	// Stdout, which is the run response text and no longer doubles as the listing.
	Models []FakeModel
}

// FakeModel is one {id,label} entry the fake reports for the models listing.
type FakeModel struct {
	ID    string
	Label string
}

// version returns the version this fake reports, defaulting to the minimum the
// manager accepts so an ordinary fake just works.
func (cfg FakeAgy) version() string {
	if cfg.Version == "" {
		return agyver.Required.String()
	}
	return cfg.Version
}

// subcommandPreamble makes the script answer the non-print invocations before
// falling through to the print-mode event stream, mirroring the real binary:
//
//   - `agy --version`, which the manager probes once before it will run
//     anything. Without this every manager-level test would fail in the version
//     gate rather than exercising its actual subject.
//   - `agy --output-format json models`, the invocation ListModels makes: agy's
//     global --output-format flag precedes the `models` subcommand, so the match
//     is on $1 and $3. modelsPath holds the JSON envelope rendered from Models,
//     and the configured Stderr and Exit apply so a failing listing can be
//     exercised too. The real binary prints its progress banner on stderr, which
//     is why ListModels reads stdout alone. A run invocation never matches: it
//     leads with --dangerously-skip-permissions, not --output-format.
func (cfg FakeAgy) subcommandPreamble(modelsPath, errPath string) string {
	return fmt.Sprintf("if [ \"$1\" = \"--version\" ]; then printf '%%s\\n' %q; exit 0; fi\n", cfg.version()) +
		fmt.Sprintf("if [ \"$1\" = \"--output-format\" ] && [ \"$3\" = \"models\" ]; then cat %q; cat %q 1>&2; exit %d; fi\n", modelsPath, errPath, cfg.Exit)
}

// modelsEnvelopeJSON renders the JSON envelope agy 1.1.12+ prints for
// `agy --output-format json models`: a SUCCESS `models` command whose
// command.data.models carries one {id,label} object per configured model. The
// inner slice is always non-nil so an empty catalog marshals as "models":[]
// rather than "models":null, matching the real binary and letting ListModels'
// decoder see a valid empty list rather than a framing it rejects.
func (cfg FakeAgy) modelsEnvelopeJSON(t *testing.T) string {
	t.Helper()
	type modelObj struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	models := make([]modelObj, 0, len(cfg.Models))
	for _, mo := range cfg.Models {
		models = append(models, modelObj(mo))
	}
	var env struct {
		Status  string `json:"status"`
		Command struct {
			Name string `json:"name"`
			Data struct {
				Models []modelObj `json:"models"`
			} `json:"data"`
		} `json:"command"`
	}
	env.Status = "SUCCESS"
	env.Command.Name = "models"
	env.Command.Data.Models = models
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal fake agy models envelope: %v", err)
	}
	return string(b)
}

// ConvID returns the conversation id this fake reports, applying the same
// defaulting the emitted stream does. Tests use it to assert without repeating
// the default.
func (cfg FakeAgy) ConvID() string {
	switch {
	case cfg.NoConversationID:
		return ""
	case cfg.ConversationID != "":
		return cfg.ConversationID
	}
	return defaultFakeConversationID
}

// result renders the terminal result this run produces, applying the status
// defaulting that the stream's result event and result.json share. streamLines
// embeds it in the result event and ResultJSON marshals it directly, so routing
// both through here keeps them from drifting: fakesupervisor.go stages ResultJSON
// as the payload the real supervisor would derive from streamLines' result event,
// and nothing else enforces that the two agree.
func (cfg FakeAgy) result() streamjson.Result {
	status := cfg.Status
	if status == "" {
		status = streamjson.StatusSuccess
	}
	return streamjson.Result{
		ConversationID: cfg.ConvID(),
		Status:         status,
		Response:       cfg.Stdout,
		Error:          cfg.ResultError,
		NumTurns:       1,
		Usage:          &streamjson.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
}

// streamLines renders the configured run as agy's stream-json output, which
// WriteFakeAgy stages as the script's stdout payload. It is unexported because the
// only consumer is in this file. One candidate does exist if anyone wants it back:
// supervisor_windows_test.go hand-rolls an equivalent three-event stream.
func (cfg FakeAgy) streamLines(t *testing.T) string {
	t.Helper()
	convID := cfg.ConvID()

	events := []streamjson.Event{{
		Kind:           streamjson.EventInit,
		ConversationID: convID,
		Init:           &streamjson.Init{Cwd: ".", PermissionMode: "request-review"},
	}, {
		Kind: streamjson.EventStepUpdate,
		StepUpdate: &streamjson.StepUpdate{
			ConversationID: convID,
			StepIndex:      0,
			State:          "DONE",
			StepType:       streamjson.StepTypeAgentResponse,
			TextDelta:      cfg.Stdout,
		},
	}}
	if !cfg.OmitResult {
		res := cfg.result()
		events = append(events, streamjson.Event{Kind: streamjson.EventResult, Result: &res})
	}

	var sb strings.Builder
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal fake agy event: %v", err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	return sb.String()
}

// ResultJSON renders the terminal result payload the real supervisor would
// derive from this run's stream and write to the job's result.json.
func (cfg FakeAgy) ResultJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(cfg.result())
	if err != nil {
		t.Fatalf("marshal fake agy result: %v", err)
	}
	return string(b)
}

// ProgressJSON renders the progress file the real supervisor would have written
// by the time this run's stream reached its agent_response step.
func (cfg FakeAgy) ProgressJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(jobstore.Progress{
		ConversationID: cfg.ConvID(),
		StepType:       streamjson.StepTypeAgentResponse,
		UpdatedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal fake agy progress: %v", err)
	}
	return string(b)
}

// WriteFakeAgy writes an executable shell script that mimics agy's stream-json
// print mode and returns its path. The script is created under t.TempDir(). The
// rendered stdout and the configured stderr are written to sibling payload files
// and reproduced faithfully via cat, so arbitrary byte content (newlines, shell
// metacharacters) survives intact.
func WriteFakeAgy(t *testing.T, cfg FakeAgy) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-agy")

	if cfg.IgnoreSIGTERM {
		// Trap and ignore SIGTERM, then loop forever. The inner sleep is killed by
		// the supervisor's group SIGTERM, but bash ignores it and restarts the
		// loop, so the process survives until the supervisor escalates to SIGKILL.
		// The version probe still has to answer, or the job never starts. There is
		// no listing or stderr to serve in this mode, so point those at paths that
		// do not exist: `agy models` is never called on a hang-forever fake.
		script := "#!/usr/bin/env bash\n" +
			cfg.subcommandPreamble(filepath.Join(dir, "no-models"), filepath.Join(dir, "no-stderr")) +
			"trap '' TERM\nwhile :; do sleep 1; done\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("write fake agy: %v", err)
		}
		return path
	}

	outPath := filepath.Join(dir, "fake-agy.out")
	errPath := filepath.Join(dir, "fake-agy.err")
	modelsPath := filepath.Join(dir, "fake-agy.models")
	if err := os.WriteFile(outPath, []byte(cfg.streamLines(t)), 0o644); err != nil {
		t.Fatalf("write fake agy stdout: %v", err)
	}
	if err := os.WriteFile(errPath, []byte(cfg.Stderr), 0o644); err != nil {
		t.Fatalf("write fake agy stderr: %v", err)
	}
	if err := os.WriteFile(modelsPath, []byte(cfg.modelsEnvelopeJSON(t)), 0o644); err != nil {
		t.Fatalf("write fake agy models envelope: %v", err)
	}
	script := fmt.Sprintf(`#!/usr/bin/env bash
%ssleep %.3f
cat %q
cat %q 1>&2
exit %d
`, cfg.subcommandPreamble(modelsPath, errPath), cfg.Sleep.Seconds(), outPath, errPath, cfg.Exit)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	return path
}

// FakeVersion returns a stand-in for the manager's `agy --version` probe,
// reporting v for any binary path. It lets a manager test exercise the version
// gate (and satisfy it) without a real agy on disk.
func FakeVersion(v string) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) { return v, nil }
}
