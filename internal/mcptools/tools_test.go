package mcptools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

// TestToStartRequestRejectsExcessiveTimeout: a client timeout is validated
// positive but must also be capped, so a typo like "1000h" cannot become both
// the agy --print-timeout and a weeks-long supervisor hard-kill window.
func TestToStartRequestRejectsExcessiveTimeout(t *testing.T) {
	t.Parallel()
	_, err := runInput{Prompt: "x", Timeout: "1000h"}.toStartRequest()
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("err = %v, want an excessive-timeout rejection", err)
	}
}

// TestToStartRequestTimeoutErrors covers both halves of the timeout check. A
// malformed value keeps ParseDuration's own message, which names what is wrong
// with the input; a well-formed but non-positive value gets the hint instead,
// since ParseDuration has nothing to say about it.
//
// The malformed rows take their expectation from time.ParseDuration itself
// rather than from a copy of its wording. The claim under test is "the parse
// error survives", not "the stdlib phrases it this way", and a hardcoded copy
// would fail on a stdlib reword for a reason unrelated to this package. They
// also assert the error still unwraps, which is what %w buys and what a bare
// %v would silently take away.
func TestToStartRequestTimeoutErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, timeout string
		wrapsParseErr bool
	}{
		{name: "missing unit", timeout: "10", wrapsParseErr: true},
		{name: "not a duration", timeout: "soon", wrapsParseErr: true},
		{name: "zero", timeout: "0s"},
		{name: "negative", timeout: "-5m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := runInput{Prompt: "x", Timeout: tc.timeout}.toStartRequest()
			if err == nil {
				t.Fatalf("timeout %q was accepted, want a rejection", tc.timeout)
			}
			if !strings.Contains(err.Error(), tc.timeout) {
				t.Fatalf("err = %v, want it to quote the rejected value %q", err, tc.timeout)
			}
			if !tc.wrapsParseErr {
				if !strings.Contains(err.Error(), "want a positive Go duration") {
					t.Fatalf("err = %v, want the positive-duration hint", err)
				}
				return
			}
			_, parseErr := time.ParseDuration(tc.timeout)
			if parseErr == nil {
				t.Fatalf("test input %q parses cleanly; it cannot exercise the parse-error branch", tc.timeout)
			}
			if !strings.Contains(err.Error(), parseErr.Error()) {
				t.Fatalf("err = %v, want it to carry the parse error %v", err, parseErr)
			}
			if errors.Unwrap(err) == nil {
				t.Fatalf("err = %v, want the parse error wrapped with %%w so errors.Is/As reach it", err)
			}
		})
	}
}

// TestParseWaitErrors is the table above's counterpart for the wait parameter,
// kept beside it on purpose. The two validators exist to give a caller the same
// quality of answer for wait as for timeout, and issue #88 was first fixed on
// only one of them; a change that improves one and forgets the other now fails
// here instead of shipping as an asymmetry.
func TestParseWaitErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, wait    string
		wrapsParseErr bool
	}{
		{name: "missing unit", wait: "10", wrapsParseErr: true},
		{name: "not a duration", wait: "nope", wrapsParseErr: true},
		{name: "zero", wait: "0s"},
		{name: "negative", wait: "-1s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseWait(tc.wait)
			if err == nil {
				t.Fatalf("wait %q was accepted, want a rejection", tc.wait)
			}
			if !strings.Contains(err.Error(), "invalid wait") || !strings.Contains(err.Error(), tc.wait) {
				t.Fatalf("err = %v, want it to say invalid wait and quote %q", err, tc.wait)
			}
			if !tc.wrapsParseErr {
				if !strings.Contains(err.Error(), "want a positive Go duration") {
					t.Fatalf("err = %v, want the positive-duration hint", err)
				}
				return
			}
			_, parseErr := time.ParseDuration(tc.wait)
			if parseErr == nil {
				t.Fatalf("test input %q parses cleanly; it cannot exercise the parse-error branch", tc.wait)
			}
			if !strings.Contains(err.Error(), parseErr.Error()) {
				t.Fatalf("err = %v, want it to carry the parse error %v", err, parseErr)
			}
			if errors.Unwrap(err) == nil {
				t.Fatalf("err = %v, want the parse error wrapped with %%w so errors.Is/As reach it", err)
			}
		})
	}
}

func TestToStartRequestAcceptsTimeoutAtLimit(t *testing.T) {
	req, err := runInput{Prompt: "x", Timeout: maxJobTimeout.String()}.toStartRequest()
	if err != nil || req.Timeout != maxJobTimeout {
		t.Fatalf("timeout at the limit should be accepted: req=%+v err=%v", req, err)
	}
}

// TestToStartRequestPassesJSONSchema: the json_schema input is threaded verbatim
// into the start request (agy-mcp does not parse or validate it, agy owns schema
// semantics), so buildAgyArgs can forward it as --json-schema. A dropped field
// here would silently ignore a caller's schema request.
func TestToStartRequestPassesJSONSchema(t *testing.T) {
	t.Parallel()
	const schema = `{"type":"object","required":["verdict"]}`
	req, err := runInput{Prompt: "x", JSONSchema: schema}.toStartRequest()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.JSONSchema != schema {
		t.Fatalf("JSONSchema = %q, want it threaded through unchanged as %q", req.JSONSchema, schema)
	}
}

// TestToStartRequestPassesRunOptions: the effort/mode/agent/sandbox inputs are
// threaded into the start request so buildAgyArgs can forward them to agy. A
// dropped field here would silently ignore a caller's run-shaping request.
func TestToStartRequestPassesRunOptions(t *testing.T) {
	t.Parallel()
	req, err := runInput{Prompt: "x", Effort: "high", Mode: "plan", Agent: "reviewer", Sandbox: true}.toStartRequest()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Effort != "high" {
		t.Errorf("Effort = %q, want high", req.Effort)
	}
	if req.Mode != "plan" {
		t.Errorf("Mode = %q, want plan", req.Mode)
	}
	if req.Agent != "reviewer" {
		t.Errorf("Agent = %q, want reviewer", req.Agent)
	}
	if !req.Sandbox {
		t.Errorf("Sandbox = %v, want true", req.Sandbox)
	}
}

// TestToStartRequestPassesModel: model is threaded into the start request
// verbatim, where the manager reduces it (a whole `agy models` row becomes its id)
// and buildAgyArgs forwards it as --model. This layer deliberately does not
// validate or rewrite it, since agy owns the model namespace and the reduction
// has to sit below the AGY_MCP_DEFAULT_MODEL fallback to cover that too.
//
// It completes the pass-through family above: model was the one run option with no
// test here, so dropping `Model: in.Model` from toStartRequest left every test in
// the repo green while silently ignoring a caller's model choice.
func TestToStartRequestPassesModel(t *testing.T) {
	t.Parallel()
	req, err := runInput{Prompt: "x", Model: "gemini-3.1-pro-high"}.toStartRequest()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Model != "gemini-3.1-pro-high" {
		t.Errorf("Model = %q, want it threaded through unchanged", req.Model)
	}
}

// TestToStartRequestValidatesMode: mode is an agy enum (accept-edits, plan), so a
// bad value must fail fast at the tool boundary with a message that quotes the bad
// value and names the accepted values, rather than reaching agy. An empty mode is
// valid and means "omit the flag". Validation is exact, so a wrong case is rejected.
func TestToStartRequestValidatesMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode string
		wantErr    bool
	}{
		{name: "empty is ok", mode: "", wantErr: false},
		{name: "accept-edits ok", mode: "accept-edits", wantErr: false},
		{name: "plan ok", mode: "plan", wantErr: false},
		{name: "unknown rejected", mode: "planning", wantErr: true},
		{name: "wrong case rejected", mode: "Plan", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := runInput{Prompt: "x", Mode: tc.mode}.toStartRequest()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mode %q was accepted, want a rejection", tc.mode)
				}
				if !strings.Contains(err.Error(), tc.mode) ||
					!strings.Contains(err.Error(), "accept-edits") ||
					!strings.Contains(err.Error(), "plan") {
					t.Fatalf("err = %v, want it to quote %q and name the accepted values", err, tc.mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("mode %q was rejected, want accepted: %v", tc.mode, err)
			}
			if req.Mode != tc.mode {
				t.Fatalf("Mode = %q, want %q", req.Mode, tc.mode)
			}
		})
	}
}

// TestToStartRequestValidatesEffort: effort is an agy enum (low, medium, high),
// so a bad value must fail fast at the tool boundary with a message that quotes
// the bad value and names the accepted values, rather than reaching agy. An empty
// effort is valid and means "omit the flag". Validation is exact, so a wrong case
// is rejected.
func TestToStartRequestValidatesEffort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, effort string
		wantErr      bool
	}{
		{name: "empty is ok", effort: "", wantErr: false},
		{name: "low ok", effort: "low", wantErr: false},
		{name: "medium ok", effort: "medium", wantErr: false},
		{name: "high ok", effort: "high", wantErr: false},
		{name: "unknown rejected", effort: "maximum", wantErr: true},
		{name: "wrong case rejected", effort: "High", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := runInput{Prompt: "x", Effort: tc.effort}.toStartRequest()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("effort %q was accepted, want a rejection", tc.effort)
				}
				if !strings.Contains(err.Error(), tc.effort) ||
					!strings.Contains(err.Error(), "low") ||
					!strings.Contains(err.Error(), "medium") ||
					!strings.Contains(err.Error(), "high") {
					t.Fatalf("err = %v, want it to quote %q and name the accepted values", err, tc.effort)
				}
				return
			}
			if err != nil {
				t.Fatalf("effort %q was rejected, want accepted: %v", tc.effort, err)
			}
			if req.Effort != tc.effort {
				t.Fatalf("Effort = %q, want %q", req.Effort, tc.effort)
			}
		})
	}
}

// TestStatusOutputModelEchoAndOmitempty: toStatusOutput echoes the resolved model
// onto the wire (issue #138 item 2), and omits the key entirely when the model is
// empty, so the field never becomes a required member of the generated schema
// (cf. #138 item 8).
func TestStatusOutputModelEchoAndOmitempty(t *testing.T) {
	got := toStatusOutput(manager.Status{State: manager.StateDone, Model: "gemini-3.1-pro-high"})
	if got.Model != "gemini-3.1-pro-high" {
		t.Errorf("Model = %q, want it echoed from the status", got.Model)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"model":"gemini-3.1-pro-high"`) {
		t.Errorf("marshaled output missing model: %s", b)
	}
	b2, err := json.Marshal(toStatusOutput(manager.Status{State: manager.StateDone}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b2), `"model"`) {
		t.Errorf("an empty model must be omitted from the wire, got: %s", b2)
	}
}

// TestStatusOutputRecoveryHint: toStatusOutput sets the recovery hint only when a
// run ended terminally with no text to offer but a conversation worth continuing,
// the timeout/cancel-before-any-output case from issue #151. A run that produced
// text (even a partial one), has no conversation_id, is still running, or finished
// successfully gets no hint, and the omitempty key is absent from the wire then.
func TestStatusOutputRecoveryHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		st       manager.Status
		wantHint bool
	}{
		{"timeout kill, no text, has conversation", manager.Status{State: manager.StateFailed, ConversationID: "c1"}, true},
		{"cancel before any output", manager.Status{State: manager.StateCancelled, ConversationID: "c1"}, true},
		{"failed but no conversation to continue", manager.Status{State: manager.StateFailed}, false},
		{"failed with a partial result already offered", manager.Status{State: manager.StateFailed, ConversationID: "c1", Result: "as far as I got", Partial: true}, false},
		{"done and successful needs no recovery", manager.Status{State: manager.StateDone, ConversationID: "c1"}, false},
		{"running is not terminal", manager.Status{State: manager.StateRunning, ConversationID: "c1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := toStatusOutput(tt.st)
			if (got.Recovery != "") != tt.wantHint {
				t.Errorf("Recovery = %q, want hint present = %v", got.Recovery, tt.wantHint)
			}
			if tt.wantHint && !strings.Contains(got.Recovery, "conversation_id") {
				t.Errorf("a recovery hint must tell the caller to reuse conversation_id, got %q", got.Recovery)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), `"recovery"`) != tt.wantHint {
				t.Errorf("recovery key on wire = %v, want %v: %s", strings.Contains(string(b), `"recovery"`), tt.wantHint, b)
			}
		})
	}
}

// TestStatusOutputFailureReason: toStatusOutput echoes FailureReason to the wire
// (omitempty, so absent unless set), and a quota wall gets its own recovery
// advice, which takes priority over the generic issue #151 hint and points the
// caller at the reset time rather than at continuing a lost thread.
func TestStatusOutputFailureReason(t *testing.T) {
	t.Parallel()

	// The reason is echoed verbatim and the omitempty key is absent when unset.
	failed := toStatusOutput(manager.Status{State: manager.StateFailed, FailureReason: manager.ReasonAgyError, Error: "model unavailable"})
	if failed.FailureReason != manager.ReasonAgyError {
		t.Errorf("FailureReason = %q, want %q", failed.FailureReason, manager.ReasonAgyError)
	}
	done, err := json.Marshal(toStatusOutput(manager.Status{State: manager.StateDone}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(done), `"failure_reason"`) {
		t.Errorf("failure_reason key must be absent on a clean done: %s", done)
	}

	// Quota recovery: present, names the transient/reset nature, and reuses the
	// conversation when one exists.
	withConv := toStatusOutput(manager.Status{
		State: manager.StateFailed, FailureReason: manager.ReasonQuotaExhausted,
		Error: "Individual quota reached. Resets in 21m50s.", ConversationID: "c1",
	})
	if withConv.Recovery == "" {
		t.Fatal("a quota wall must carry recovery advice")
	}
	if !strings.Contains(withConv.Recovery, "reset") {
		t.Errorf("quota recovery should point at the reset, got %q", withConv.Recovery)
	}
	if !strings.Contains(withConv.Recovery, "conversation_id") {
		t.Errorf("quota recovery with a conversation should tell the caller to reuse it, got %q", withConv.Recovery)
	}

	// A quota wall with no conversation still gets advice (wait then retry the
	// run), where the generic issue #151 hint would have stayed silent.
	noConv := toStatusOutput(manager.Status{
		State: manager.StateFailed, FailureReason: manager.ReasonQuotaExhausted,
		Error: "Individual quota reached. Resets in 21m50s.",
	})
	if noConv.Recovery == "" {
		t.Error("a quota wall must carry recovery advice even without a conversation_id")
	}

	// A quota wall that DID stream partial text keeps the Recovery invariant:
	// recovery is present only when there is no result, so a caller can read its
	// presence as "no usable result". The partial text is in Result, and
	// failure_reason still signals the transient wall. Before the guard, this job
	// shape flipped recovery from absent to present (a regression).
	withText := toStatusOutput(manager.Status{
		State: manager.StateFailed, FailureReason: manager.ReasonQuotaExhausted,
		Error:  "Individual quota reached. Resets in 21m50s.",
		Result: "partial analysis", ConversationID: "c1",
	})
	if withText.Recovery != "" {
		t.Errorf("a quota wall carrying partial text must not set recovery (result already holds the text), got %q", withText.Recovery)
	}
	if withText.Result != "partial analysis" {
		t.Errorf("partial text must be preserved in result, got %q", withText.Result)
	}
	if withText.FailureReason != manager.ReasonQuotaExhausted {
		t.Errorf("failure_reason must still signal the quota wall even with partial text, got %q", withText.FailureReason)
	}
}
