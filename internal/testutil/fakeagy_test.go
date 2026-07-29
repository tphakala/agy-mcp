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
