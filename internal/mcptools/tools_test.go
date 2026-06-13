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

func TestToStartRequestAcceptsTimeoutAtLimit(t *testing.T) {
	req, err := runInput{Prompt: "x", Timeout: maxJobTimeout.String()}.toStartRequest()
	if err != nil || req.Timeout != maxJobTimeout {
		t.Fatalf("timeout at the limit should be accepted: req=%+v err=%v", req, err)
	}
}
