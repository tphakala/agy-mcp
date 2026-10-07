package manager

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/tphakala/agy-mcp/v2/internal/agyver"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/proc"
)

// versionManager builds a manager whose agy path resolves without a real binary
// and whose version probe returns raw (or err).
func versionManager(t *testing.T, raw string, err error) (*Manager, *atomic.Int32) {
	t.Helper()
	m := New(config.Config{AgyPath: "/usr/bin/agy", StateDir: t.TempDir(), MaxConcurrency: 4})
	var calls atomic.Int32
	m.readAgyVersion = func(context.Context, string) (string, error) {
		calls.Add(1)
		return raw, err
	}
	return m, &calls
}

func TestAgyBinaryCheckedAcceptsSupportedVersion(t *testing.T) {
	m, calls := versionManager(t, agyver.Required.String()+"\n", nil)
	got, err := m.agyBinaryChecked(t.Context())
	if err != nil {
		t.Fatalf("agyBinaryChecked: %v", err)
	}
	if got != "/usr/bin/agy" {
		t.Fatalf("path = %q", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("probe ran %d times, want 1", n)
	}
}

// A verified binary is probed once per process: the whole point of caching is
// that a tool call does not pay a process spawn every time.
func TestAgyBinaryCheckedCachesSuccess(t *testing.T) {
	m, calls := versionManager(t, "1.2.0", nil)
	for range 5 {
		if _, err := m.agyBinaryChecked(t.Context()); err != nil {
			t.Fatalf("agyBinaryChecked: %v", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("probe ran %d times, want 1 (result must be cached)", n)
	}
}

func TestAgyBinaryCheckedRefusesOldVersion(t *testing.T) {
	m, _ := versionManager(t, "1.1.7", nil)
	_, err := m.agyBinaryChecked(t.Context())
	if err == nil {
		t.Fatal("an agy older than the floor must be refused")
	}
	// The message has to name both what is needed and what was found, or the
	// reader cannot tell which half of the mismatch to fix.
	for _, want := range []string{agyver.Required.String(), "1.1.7", "/usr/bin/agy", "stream-json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestAgyBinaryCheckedRefusesImmediateSubFloor pins the boundary the floor bump
// moves: agy 1.1.14, the release directly below the 1.1.15 floor and one that
// used to pass, is now refused. That is what makes raising the floor a real gate
// change rather than a constant edit; TestAgyBinaryCheckedRefusesOldVersion only
// proves an ancient 1.1.7 is refused, which was true before the bump too.
func TestAgyBinaryCheckedRefusesImmediateSubFloor(t *testing.T) {
	m, _ := versionManager(t, "1.1.14", nil)
	if _, err := m.agyBinaryChecked(t.Context()); err == nil {
		t.Fatal("agy 1.1.14 (one below the 1.1.15 floor) must be refused")
	}
}

// A refusal is deliberately not cached, so upgrading agy is picked up without
// restarting the server, matching the deferred PATH lookup's promise.
func TestAgyBinaryCheckedRetriesAfterRefusal(t *testing.T) {
	m := New(config.Config{AgyPath: "/usr/bin/agy", StateDir: t.TempDir(), MaxConcurrency: 4})
	var version atomic.Value
	version.Store("1.1.7")
	var calls atomic.Int32
	m.readAgyVersion = func(context.Context, string) (string, error) {
		calls.Add(1)
		v, _ := version.Load().(string)
		return v, nil
	}

	if _, err := m.agyBinaryChecked(t.Context()); err == nil {
		t.Fatal("precondition: the old version must be refused")
	}
	version.Store(agyver.Required.String()) // the user upgrades agy mid-session
	if _, err := m.agyBinaryChecked(t.Context()); err != nil {
		t.Fatalf("an upgraded agy must be accepted without a restart: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("probe ran %d times, want 2 (a failure must not be cached)", n)
	}
}

func TestAgyBinaryCheckedReportsUnparseableOutput(t *testing.T) {
	m, _ := versionManager(t, "who knows", nil)
	_, err := m.agyBinaryChecked(t.Context())
	if err == nil {
		t.Fatal("output with no version number must be an error")
	}
	if !strings.Contains(err.Error(), "who knows") {
		t.Errorf("error %q should quote what agy actually printed", err)
	}
}

func TestAgyBinaryCheckedReportsProbeFailure(t *testing.T) {
	m, _ := versionManager(t, "", errors.New("permission denied"))
	_, err := m.agyBinaryChecked(t.Context())
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the probe failure surfaced", err)
	}
}

// A missing agy is reported as a lookup failure, not a version failure, and
// never runs the probe: there is nothing to probe.
func TestAgyBinaryCheckedWithoutBinary(t *testing.T) {
	m := New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4})
	var calls atomic.Int32
	m.readAgyVersion = func(context.Context, string) (string, error) {
		calls.Add(1)
		return agyver.Required.String(), nil
	}
	t.Setenv("PATH", t.TempDir()) // no agy anywhere on it
	if _, err := m.agyBinaryChecked(t.Context()); err == nil {
		t.Fatal("a missing agy must be an error")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("probe ran %d times for a binary that could not be resolved", n)
	}
}

// TestVersionGateRedactsAgyErrorInUnparseableOutput: when the version output does
// not parse and holds an AGY_ERROR line, every consumer of the gate's error gets
// the reduced line, not the cloud project and region its short_error carries
// (issue #211). Every row fails at the gate before any exec, so no fake agy is
// needed.
func TestVersionGateRedactsAgyErrorInUnparseableOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(m *Manager) error
	}{
		{"agyBinaryChecked", func(m *Manager) error { _, err := m.agyBinaryChecked(t.Context()); return err }},
		{"StartJob", func(m *Manager) error {
			if !proc.Supported {
				t.Skip("StartJob refuses on platforms without job supervision before it reaches the version gate")
			}
			_, err := m.StartJob(StartRequest{Prompt: "hi", Cwd: t.TempDir()})
			return err
		}},
		{"ListModels", func(m *Manager) error { _, err := m.ListModels(t.Context()); return err }},
		{"ListAgents", func(m *Manager) error { _, err := m.ListAgents(t.Context(), ""); return err }},
		{"readUsage", func(m *Manager) error { _, err := m.readUsage(t.Context()); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := versionManager(t, measuredAgyErrorStderr, nil)
			err := tc.call(m)
			if err == nil {
				t.Fatal("unparseable version output must be an error")
			}
			msg := err.Error()
			for _, leak := range []string{"example-project", "example-region"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error leaks %q: %s", leak, msg)
				}
			}
			for _, want := range []string{"Selected model is not supported", "AGY_ERROR: NOT_FOUND (code 404)"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error lacks %q: %s", want, msg)
				}
			}
		})
	}
}

// TestVersionParseErrorNamesOutputReducedToNothing: output whose every non-blank
// line is an AGY_ERROR line the reduction drops must not quote as "", which would
// read as "agy printed nothing" (issue #211). The same holds for the doctor check.
func TestVersionParseErrorNamesOutputReducedToNothing(t *testing.T) {
	for _, raw := range []string{
		wireAgyErrorPrefix + "{not json\n",
		wireAgyErrorPrefix + `{"short_error":"oops"}` + "\n",
	} {
		m, _ := versionManager(t, raw, nil)
		_, err := m.agyBinaryChecked(t.Context())
		if err == nil {
			t.Fatalf("output %q must be an error", raw)
		}
		report := m.Doctor(t.Context())
		detail := findCheck(t, report, checkAgyVersionName).Detail
		for site, msg := range map[string]string{"agyBinaryChecked": err.Error(), "doctor": detail} {
			if !strings.Contains(msg, "reduced to nothing once AGY_ERROR lines were left out") {
				t.Errorf("%s message for %q should say the output reduced to nothing: %s", site, raw, msg)
			}
			if strings.Contains(msg, `""`) || strings.Contains(msg, "not json") {
				t.Errorf("%s message for %q quotes an empty or raw output: %s", site, raw, msg)
			}
		}
	}
}

// TestAgyBinaryCheckedParsesUnreducedOutput: the reduction can drop a version that
// shares a carriage-return-split line with an AGY_ERROR segment it cannot decode,
// so agyver.Parse must see the unreduced output and only the failure message is
// reduced (issue #211). The doctor check parses the same way.
func TestAgyBinaryCheckedParsesUnreducedOutput(t *testing.T) {
	raw := wireAgyErrorPrefix + "{not json\r" + agyver.Required.String() + "\n"
	m, _ := versionManager(t, raw, nil)
	if _, err := m.agyBinaryChecked(t.Context()); err != nil {
		t.Fatalf("agyBinaryChecked refused a parseable version: %v", err)
	}
	m, _ = versionManager(t, raw, nil)
	if got := findCheck(t, m.Doctor(t.Context()), checkAgyVersionName); got.Status != CheckPass {
		t.Fatalf("doctor version check = %v (%s), want PASS", got.Status, got.Detail)
	}
}

func TestVersionOutputForMessage(t *testing.T) {
	unquote := func(t *testing.T, s string) string {
		t.Helper()
		u, err := strconv.Unquote(s)
		if err != nil {
			t.Fatalf("result %q is not a quoted string: %v", s, err)
		}
		return u
	}
	t.Run("bounds the reduced text to its tail", func(t *testing.T) {
		got := unquote(t, versionOutputForMessage("HEAD"+strings.Repeat("x", probeErrorLimit)+"TAIL"))
		if len(got) > probeErrorLimit || !strings.HasSuffix(got, "TAIL") || strings.Contains(got, "HEAD") {
			t.Errorf("got %d bytes, want at most %d from the tail", len(got), probeErrorLimit)
		}
	})
	t.Run("reduces before it cuts", func(t *testing.T) {
		// The last probeErrorLimit bytes start 20 bytes into the JSON object, so
		// cutting first would keep a prefix-less fragment holding the project.
		line := wireAgyErrorPrefix + measuredAgyErrorJSON + "\n"
		raw := line + strings.Repeat("y", probeErrorLimit-len(line)+len(wireAgyErrorPrefix)+20)
		got := unquote(t, versionOutputForMessage(raw))
		if strings.Contains(got, "example-project") || !strings.HasPrefix(got, "AGY_ERROR: NOT_FOUND (code 404)") {
			t.Errorf("reduction did not run before the cut: %.120q", got)
		}
	})
	t.Run("keeps valid UTF-8 across the cut", func(t *testing.T) {
		got := unquote(t, versionOutputForMessage("é"+strings.Repeat("z", probeErrorLimit-1)))
		if !utf8.ValidString(got) {
			t.Error("result is not valid UTF-8")
		}
	})
	t.Run("quotes other output as before and keeps empty output empty", func(t *testing.T) {
		if got, want := versionOutputForMessage("who knows\n"), strconv.Quote("who knows"); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
		if got := versionOutputForMessage("  \n"); got != `""` {
			t.Errorf("whitespace-only output = %s, want the quoted empty string", got)
		}
	})
}

// TestVersionOutputForMessageCutsAgyErrorNotAtLineStart: readAgyVersion merges
// stdout and stderr, so an AGY_ERROR line can land after other bytes or in another
// spelling, where redactAgyErrorLines does not recognise it. The text from such a
// marker to the end of its segment is cut, line terminators are kept, and a later
// line that starts with the exact prefix is still reduced (issue #211).
func TestVersionOutputForMessageCutsAgyErrorNotAtLineStart(t *testing.T) {
	obj := measuredAgyErrorJSON
	for _, tc := range []struct {
		name string
		raw  string
		want string // the unquoted result
	}{
		{"after other text", "loading " + wireAgyErrorPrefix + obj + "\n", "loading"},
		{"lowercase at column 0", "agy_error: " + obj + "\n", `""`},
		{"without the space", "AGY_ERROR:" + obj + "\n", `""`},
		{"after an escape sequence", "\x1b[31m" + wireAgyErrorPrefix + obj + "\n", "\x1b[31m"},
		{
			"a cut line does not swallow its newline",
			"loading " + wireAgyErrorPrefix + obj + "\n" + wireAgyErrorPrefix + obj + "\n",
			"loading \nAGY_ERROR: NOT_FOUND (code 404)",
		},
		{
			"a cut segment does not swallow its carriage return",
			"loading " + wireAgyErrorPrefix + obj + "\r" + wireAgyErrorPrefix + obj + "\n",
			"loading \rAGY_ERROR: NOT_FOUND (code 404)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := versionOutputForMessage(tc.raw)
			for _, leak := range []string{"example-project", "example-region", "short_error"} {
				if strings.Contains(got, leak) {
					t.Errorf("result leaks %q: %s", leak, got)
				}
			}
			if want := strconv.Quote(tc.want); tc.want != `""` && got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		})
	}
}
