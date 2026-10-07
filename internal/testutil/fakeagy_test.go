//go:build !windows

package testutil

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/streamjson"
)

// The fake emits a stream-json event stream, not bare text: the response shows
// up as an agent_response text_delta and again in the terminal result, which is
// what the supervisor decodes.
func TestFakeAgyEmitsStreamJSON(t *testing.T) {
	cfg := FakeAgy{Stdout: "hello world", Exit: 0}
	path := WriteFakeAgy(t, cfg)
	res := runScript(t, 10*time.Second, path, "-p", "ignored")
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %q", res.ExitCode, res.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d event lines, want init + step_update + result:\n%s", len(lines), res.Stdout)
	}
	sr := streamjson.NewReader(strings.NewReader(res.Stdout))
	var kinds []string
	var response, convID string
	for {
		ev, err := sr.Next()
		if err != nil {
			break
		}
		kinds = append(kinds, ev.Kind)
		if ev.Kind == streamjson.EventInit {
			convID = ev.ConversationID
		}
		if ev.Result != nil {
			response = ev.Result.Response
		}
	}
	want := []string{streamjson.EventInit, streamjson.EventStepUpdate, streamjson.EventResult}
	if !slices.Equal(kinds, want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	if response != "hello world" {
		t.Fatalf("result response = %q, want %q", response, "hello world")
	}
	if convID != cfg.ConvID() {
		t.Fatalf("init conversation_id = %q, want %q", convID, cfg.ConvID())
	}
	if sr.Malformed() != 0 {
		t.Fatalf("%d malformed lines in a well-formed stream", sr.Malformed())
	}
}

// The manager probes `agy --version` before it will run anything, so the fake
// has to answer it without emitting its event stream.
func TestFakeAgyAnswersVersionProbe(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stdout: "unused", Version: "1.2.3"})
	res := runScript(t, 10*time.Second, path, "--version")
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stdout); got != "1.2.3" {
		t.Fatalf("stdout = %q, want the version alone", got)
	}
}

// The quota probe passes exactly four tokens. The fake answers that invocation
// with the usage envelope and must not answer a variant that adds a flag, so a
// probe regression (for instance --disable-slash-commands, which makes agy treat
// /usage as a model prompt) fails the decoder instead of passing silently.
func TestFakeAgyAnswersUsageProbe(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stdout: "unused", Usage: []FakeQuotaGroup{{
		Name: "Gemini Models",
		Buckets: []FakeQuotaBucket{{
			ID: "5h", Name: "5h limit", Window: "5h", RemainingFraction: 0.5, ResetTime: "2026-10-04T11:32:45Z",
		}},
	}}})
	res := runScript(t, 10*time.Second, path, "--output-format", "json", "-p", "/usage")
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{`"name":"usage"`, `"status":"SUCCESS"`, `"remaining_fraction":0.5`, `"Gemini Models"`} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout %q lacks %s", res.Stdout, want)
		}
	}

	res = runScript(t, 10*time.Second, path, "--output-format", "json", "--disable-slash-commands", "-p", "/usage")
	if strings.Contains(res.Stdout, `"name":"usage"`) {
		t.Fatalf("a probe with --disable-slash-commands was answered as a usage command: %q", res.Stdout)
	}
}

func TestFakeAgyNonZeroExit(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stderr: "boom", Exit: 3})
	res := runScript(t, 10*time.Second, path)
	if res.ExitCode != 3 {
		t.Fatalf("exit = %d, want 3; stderr: %q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(res.Stderr); got != "boom" {
		t.Fatalf("stderr = %q, want %q", got, "boom")
	}
}

// TestFakeAgyAppliesFractionalSleep: a sub-second Sleep must be honored as a real
// fractional delay (bash sleep accepts fractions), proving the field is a duration
// rather than whole seconds. Lower bound only, since a sleep is never shorter than
// requested; the generous slack keeps it robust on slow CI.
func TestFakeAgyAppliesFractionalSleep(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stdout: "ok", Sleep: 150 * time.Millisecond})
	start := time.Now()
	res := runScript(t, 10*time.Second, path)
	elapsed := time.Since(start)
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %q", res.ExitCode, res.Stderr)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("elapsed %s, want >= ~150ms; fractional sleep not applied", elapsed)
	}
}

// StreamChunks must actually put several deltas on the wire, all on ONE step,
// with only the last in DONE state. Asserted here rather than downstream because
// this is the only place the emitted stream is observable: a consumer that
// correctly reassembles the text cannot tell one delta from many, so a fake that
// silently ignored StreamChunks would leave every downstream test passing while
// exercising nothing.
func TestFakeAgyStreamChunksEmitsManyDeltasOnOneStep(t *testing.T) {
	const answer = "one two three four five six seven eight"
	const chunks = 4
	path := WriteFakeAgy(t, FakeAgy{Stdout: answer, StreamChunks: chunks, Exit: 0})
	res := runScript(t, 10*time.Second, path, "-p", "ignored")
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %q", res.ExitCode, res.Stderr)
	}

	sr := streamjson.NewReader(strings.NewReader(res.Stdout))
	var states []string
	var assembled strings.Builder
	indexes := map[int]bool{}
	for {
		ev, err := sr.Next()
		if err != nil {
			break
		}
		su := ev.StepUpdate
		if su == nil || su.StepType != streamjson.StepTypeAgentResponse {
			continue
		}
		states = append(states, su.State)
		indexes[su.StepIndex] = true
		assembled.WriteString(su.TextDelta)
	}
	if sr.Malformed() != 0 {
		t.Fatalf("%d malformed lines in a well-formed stream", sr.Malformed())
	}
	if len(states) != chunks {
		t.Fatalf("got %d agent_response deltas, want %d; StreamChunks was ignored", len(states), chunks)
	}
	// All on one step: that is what makes a step_index dedup drop chunks, which is
	// the regression this fake exists to expose.
	if len(indexes) != 1 {
		t.Fatalf("deltas spanned step indexes %v, want exactly one step", indexes)
	}
	want := []string{streamjson.StateActive, streamjson.StateActive, streamjson.StateActive, streamjson.StateDone}
	if !slices.Equal(states, want) {
		t.Fatalf("delta states = %v, want %v (only the tail is DONE)", states, want)
	}
	if assembled.String() != answer {
		t.Fatalf("deltas concatenated to %q, want the whole response %q", assembled.String(), answer)
	}
}

// The default (StreamChunks unset) must stay the single DONE delta every
// existing test was written against, which is also what agy does for a response
// short enough to fit one chunk (MEASURED against agy 1.1.22).
func TestFakeAgyDefaultsToOneDoneDelta(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stdout: "short answer", Exit: 0})
	res := runScript(t, 10*time.Second, path, "-p", "ignored")
	sr := streamjson.NewReader(strings.NewReader(res.Stdout))
	var states []string
	for {
		ev, err := sr.Next()
		if err != nil {
			break
		}
		if su := ev.StepUpdate; su != nil && su.StepType == streamjson.StepTypeAgentResponse {
			states = append(states, su.State)
		}
	}
	if !slices.Equal(states, []string{streamjson.StateDone}) {
		t.Fatalf("delta states = %v, want a single DONE delta", states)
	}
}

// TestFakeAgyAgentsRejectsTrailingFlag: real agy rejects --add-dir after the
// agents subcommand (MEASURED against agy 1.3.1), so the fake must not serve the
// listing for that shape; it falls through to the run stream, which is not an
// agents envelope.
func TestFakeAgyAgentsRejectsTrailingFlag(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Agents: []string{"global"}})

	ok := runScript(t, 10*time.Second, path, "--output-format", "json", "--add-dir", "/w", "agents")
	if !strings.Contains(ok.Stdout, `"agents":["global"]`) {
		t.Fatalf("leading --add-dir: stdout = %q, want the agents envelope", ok.Stdout)
	}
	bad := runScript(t, 10*time.Second, path, "--output-format", "json", "agents", "--add-dir", "/w")
	if strings.Contains(bad.Stdout, `"agents":["global"]`) {
		t.Fatalf("trailing --add-dir: stdout = %q, want no agents envelope", bad.Stdout)
	}
}
