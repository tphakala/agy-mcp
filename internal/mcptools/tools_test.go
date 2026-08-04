package mcptools

import (
	"errors"
	"strings"
	"testing"
	"time"
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

// TestToStartRequestPassesRunOptions: the mode/agent/sandbox inputs are threaded
// into the start request so buildAgyArgs can forward them to agy. A dropped field
// here would silently ignore a caller's run-shaping request.
func TestToStartRequestPassesRunOptions(t *testing.T) {
	t.Parallel()
	req, err := runInput{Prompt: "x", Mode: "plan", Agent: "reviewer", Sandbox: true}.toStartRequest()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
