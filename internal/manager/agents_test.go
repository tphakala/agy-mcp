package manager

import (
	"slices"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// TestListAgentsReturnsConfiguredNames: the happy path decodes the agent names
// from command.data.agents, in order, as plain strings (no id/label reduction).
func TestListAgentsReturnsConfiguredNames(t *testing.T) {
	skipIfWindows(t, "Unix-specific: WriteFakeAgy emits a bash script that is not executable on Windows; ListAgents decoding is covered on Linux")
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Agents: []string{"reviewer", "researcher"}})
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})

	got, err := m.ListAgents(t.Context())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if want := []string{"reviewer", "researcher"}; !slices.Equal(got, want) {
		t.Fatalf("ListAgents() = %#v, want %#v", got, want)
	}
}

// TestListAgentsEmptyCatalog: no agents configured is the common state and must
// be a clean empty (non-nil) list, not an error.
func TestListAgentsEmptyCatalog(t *testing.T) {
	skipIfWindows(t, "Unix-specific: WriteFakeAgy emits a bash script that is not executable on Windows")
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{})
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})

	got, err := m.ListAgents(t.Context())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("ListAgents() = %#v, want a non-nil empty slice", got)
	}
}

// TestListAgentsIncludesStderrOnError: when `agy agents` fails, the error must
// carry agy's stderr (e.g. an auth prompt) rather than a bare "exit status 1".
func TestListAgentsIncludesStderrOnError(t *testing.T) {
	skipIfWindows(t, "Unix-specific: WriteFakeAgy emits a bash script that is not executable on Windows")
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: "agy: not logged in", Exit: 1})
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})

	_, err := m.ListAgents(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err = %v, want it to include agy's stderr", err)
	}
}

// TestDecodeAgentsEnvelope pins how agy's JSON agents envelope becomes the name
// list: the names come from command.data.agents, an empty array is a valid empty
// catalog, and a malformed or wrong-shaped envelope is an error rather than a
// silent empty list (what keeps a future field rename loud, mirroring the models
// decoder).
func TestDecodeAgentsEnvelope(t *testing.T) {
	t.Parallel()

	// A normal envelope, with the extra top-level fields agy also emits (response,
	// usage) present to prove the decoder reads command.data.agents and ignores them.
	const twoAgents = `{"status":"SUCCESS","response":"reviewer\nresearcher\n","usage":{"total_tokens":0},` +
		`"command":{"name":"agents","data":{"agents":["reviewer","researcher"]}}}`
	got, err := decodeAgentsEnvelope([]byte(twoAgents))
	if err != nil {
		t.Fatalf("decodeAgentsEnvelope: %v", err)
	}
	if want := []string{"reviewer", "researcher"}; !slices.Equal(got, want) {
		t.Fatalf("decodeAgentsEnvelope() = %#v, want %#v", got, want)
	}

	// An empty agents array is a legitimately empty catalog: no error, and a
	// non-nil empty slice so the list_agents handler need not special-case nil.
	const emptyCatalog = `{"status":"SUCCESS","command":{"name":"agents","data":{"agents":[]}}}`
	got, err = decodeAgentsEnvelope([]byte(emptyCatalog))
	if err != nil {
		t.Fatalf("decodeAgentsEnvelope (empty): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("decodeAgentsEnvelope (empty) = %#v, want a non-nil empty slice", got)
	}

	// Shapes that must be a loud error, not a silent empty catalog. Each pins a
	// specific guard: without its check the payload decodes to a (possibly empty)
	// agents list with err nil, and that case would go green. The last three cover
	// a renamed or dropped inner array (absent data, absent agents, null agents),
	// which must be distinguished from a present "agents":[] empty catalog above.
	for _, tc := range []struct{ name, raw string }{
		{"malformed json", `{"status":"SUCCESS","command":`},
		{"status not SUCCESS", `{"status":"ERROR","command":{"name":"agents","data":{"agents":["x"]}}}`},
		{"wrong command", `{"status":"SUCCESS","command":{"name":"run","data":{"agents":["x"]}}}`},
		{"missing data object", `{"status":"SUCCESS","command":{"name":"agents"}}`},
		{"missing agents array", `{"status":"SUCCESS","command":{"name":"agents","data":{}}}`},
		{"null agents array", `{"status":"SUCCESS","command":{"name":"agents","data":{"agents":null}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeAgentsEnvelope([]byte(tc.raw)); err == nil {
				t.Fatalf("decodeAgentsEnvelope(%s) succeeded, want an error", tc.name)
			}
		})
	}
}
