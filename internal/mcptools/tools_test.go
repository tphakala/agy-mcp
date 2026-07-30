package mcptools

import (
	"strings"
	"testing"
)

// TestToStartRequestRejectsExcessiveTimeout: a client timeout is validated
// positive but must also be capped, so a typo like "1000h" cannot become both
// the agy --print-timeout and a weeks-long supervisor hard-kill window.
func TestToStartRequestRejectsExcessiveTimeout(t *testing.T) {
	_, err := runInput{Prompt: "x", Timeout: "1000h"}.toStartRequest()
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("err = %v, want an excessive-timeout rejection", err)
	}
}

// TestToStartRequestTimeoutErrors covers both halves of the timeout check. A
// malformed value keeps ParseDuration's own message, which names what is wrong
// with the input; a well-formed but non-positive value gets the hint instead,
// since ParseDuration has nothing to say about it.
func TestToStartRequestTimeoutErrors(t *testing.T) {
	for _, tc := range []struct{ name, timeout, want string }{
		{"missing unit", "10", `missing unit in duration "10"`},
		{"not a duration", "soon", `invalid duration "soon"`},
		{"zero", "0s", "want a positive Go duration"},
		{"negative", "-5m", "want a positive Go duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runInput{Prompt: "x", Timeout: tc.timeout}.toStartRequest()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.timeout) {
				t.Fatalf("err = %v, want it to quote the rejected value %q", err, tc.timeout)
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
