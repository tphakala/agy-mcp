package manager

import (
	"slices"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// TestListModelsIncludesStderrOnError: when `agy models` fails, the error must
// carry agy's stderr (e.g. an auth prompt) rather than a bare "exit status 1".
func TestListModelsIncludesStderrOnError(t *testing.T) {
	skipIfWindows(t, "Unix-specific: WriteFakeAgy emits a bash script that is not executable on Windows; ListModels error-surfacing is covered on Linux")
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: "agy: not logged in", Exit: 1})
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})

	_, err := m.ListModels(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err = %v, want it to include agy's stderr", err)
	}
}

// A whitespace-only stderr is not a message: appending it would produce
// "agy models: exit status 1: " with nothing after the separator, the same
// dangling-separator shape errorSummary and spawnFailMessage were fixed for.
// TestListModelsIncludesStderrOnError only covers a stderr with content, so
// without this the trim is unpinned.
func TestListModelsOmitsWhitespaceOnlyStderr(t *testing.T) {
	skipIfWindows(t, "Unix-specific: WriteFakeAgy emits a bash script that is not executable on Windows")
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stderr: " \n\t ", Exit: 1})
	m := New(config.Config{AgyPath: agy, StateDir: t.TempDir(), MaxConcurrency: 4})

	_, err := m.ListModels(t.Context())
	if err == nil {
		t.Fatal("ListModels must fail when agy exits non-zero")
	}
	if strings.HasSuffix(err.Error(), ": ") || strings.HasSuffix(err.Error(), ":") {
		t.Fatalf("err = %q, ends in a dangling separator with no message after it", err)
	}
	// Positive control: the failure itself must still be reported, so the trim
	// cannot pass by swallowing the error entirely.
	if !strings.Contains(err.Error(), "agy models:") {
		t.Fatalf("err = %q, want it to name the failing command", err)
	}
}

// TestDecodeModelsEnvelope pins how agy's JSON models envelope becomes Models:
// the id/label pairs come from command.data.models, a label-less entry keeps an
// empty label rather than being dropped, an empty models array is a valid empty
// catalog, and a malformed or wrong-shaped envelope is an error rather than a
// silent empty list (the #135 class, and what keeps a future field rename loud).
func TestDecodeModelsEnvelope(t *testing.T) {
	t.Parallel()

	// A normal envelope, with the extra top-level fields agy also emits (response,
	// usage) present to prove the decoder reads command.data.models and ignores them.
	const twoModels = `{"status":"SUCCESS","response":"gemini-3.1-pro-high\tGemini 3.1 Pro (High)\n","usage":{"total_tokens":0},` +
		`"command":{"name":"models","data":{"models":[` +
		`{"id":"gemini-3.1-pro-high","label":"Gemini 3.1 Pro (High)"},` +
		`{"id":"claude-sonnet-4-6","label":"Claude Sonnet 4.6 (Thinking)"}]}}}`
	got, err := decodeModelsEnvelope([]byte(twoModels))
	if err != nil {
		t.Fatalf("decodeModelsEnvelope: %v", err)
	}
	want := []Model{
		{ID: "gemini-3.1-pro-high", Label: "Gemini 3.1 Pro (High)"},
		{ID: "claude-sonnet-4-6", Label: "Claude Sonnet 4.6 (Thinking)"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("decodeModelsEnvelope() = %#v, want %#v", got, want)
	}

	// A model object with no label keeps an empty label rather than being dropped.
	const noLabel = `{"status":"SUCCESS","command":{"name":"models","data":{"models":[{"id":"some-unlabelled-model"}]}}}`
	got, err = decodeModelsEnvelope([]byte(noLabel))
	if err != nil {
		t.Fatalf("decodeModelsEnvelope (no label): %v", err)
	}
	if want := []Model{{ID: "some-unlabelled-model"}}; !slices.Equal(got, want) {
		t.Fatalf("decodeModelsEnvelope (no label) = %#v, want %#v", got, want)
	}

	// An empty models array is a legitimately empty catalog: no error, and a
	// non-nil empty slice so the list_models handler need not special-case nil.
	const emptyCatalog = `{"status":"SUCCESS","command":{"name":"models","data":{"models":[]}}}`
	got, err = decodeModelsEnvelope([]byte(emptyCatalog))
	if err != nil {
		t.Fatalf("decodeModelsEnvelope (empty): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("decodeModelsEnvelope (empty) = %#v, want a non-nil empty slice", got)
	}

	// Shapes that must be a loud error, not a silent empty catalog. Each pins a
	// specific guard: without its check the payload decodes to a (possibly empty)
	// models list with err nil, and that case would go green. The last three cover
	// a renamed or dropped inner array (absent data, absent models, null models),
	// which must be distinguished from a present "models":[] empty catalog above.
	for _, tc := range []struct{ name, raw string }{
		{"malformed json", `{"status":"SUCCESS","command":`},
		{"status not SUCCESS", `{"status":"ERROR","command":{"name":"models","data":{"models":[{"id":"x"}]}}}`},
		{"wrong command", `{"status":"SUCCESS","command":{"name":"run","data":{"models":[{"id":"x"}]}}}`},
		{"missing data object", `{"status":"SUCCESS","command":{"name":"models"}}`},
		{"missing models array", `{"status":"SUCCESS","command":{"name":"models","data":{}}}`},
		{"null models array", `{"status":"SUCCESS","command":{"name":"models","data":{"models":null}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeModelsEnvelope([]byte(tc.raw)); err == nil {
				t.Fatalf("decodeModelsEnvelope(%s) succeeded, want an error", tc.name)
			}
		})
	}
}

// TestValidateListingFraming pins the shared framing validator that both listing
// decoders delegate to: a matching SUCCESS envelope passes, while a non-SUCCESS
// status and a mismatched command name each error with the command as the message
// prefix (so a failure names the listing it came from). decodeModelsEnvelope and
// decodeAgentsEnvelope exercise it through real payloads; this pins it directly.
func TestValidateListingFraming(t *testing.T) {
	if err := validateListingFraming("models", "SUCCESS", "models"); err != nil {
		t.Fatalf("a SUCCESS models envelope must validate, got: %v", err)
	}
	if err := validateListingFraming("agents", "ERROR", "agents"); err == nil ||
		!strings.Contains(err.Error(), "agy agents:") || !strings.Contains(err.Error(), "status") {
		t.Fatalf("a non-SUCCESS status must error with the command prefix, got: %v", err)
	}
	if err := validateListingFraming("models", "SUCCESS", "run"); err == nil ||
		!strings.Contains(err.Error(), "agy models:") || !strings.Contains(err.Error(), "command") {
		t.Fatalf("a mismatched command name must error with the command prefix, got: %v", err)
	}
}

// TestModelID pins which part of a model value reaches agy: the id column only,
// with anything that is not a whole `agy models` row forwarded untouched because
// agy owns the model namespace (issue #135).
func TestModelID(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"whole row keeps only the id", "gemini-3.1-pro-high\tGemini 3.1 Pro (High)", "gemini-3.1-pro-high"},
		{"bare id is unchanged", "gemini-3.1-pro-high", "gemini-3.1-pro-high"},
		{"bare label is forwarded untouched", "Gemini 3.1 Pro (High)", "Gemini 3.1 Pro (High)"},
		{"empty stays empty so the flag is omitted", "", ""},
		{"only the first tab cuts", "gemini-3.1-pro-high\tGemini 3.1 Pro\t(High)", "gemini-3.1-pro-high"},
		// splitModelRow trims each column, so even a padded copy of a row reduces to
		// the bare id agy accepts.
		{"padding is trimmed", "  gemini-3.1-pro-high  ", "gemini-3.1-pro-high"},
		{"padded row reduces to the trimmed id", "  gemini-3.1-pro-high \tGemini 3.1 Pro (High)", "gemini-3.1-pro-high"},
		// An empty id column must never collapse to "": buildAgyArgs omits --model
		// for an empty value, so the run would silently use agy's own default
		// instead of failing on a value the caller actually supplied.
		{"leading tab does not empty a non-empty value", "\tGemini 3.1 Pro (High)", "Gemini 3.1 Pro (High)"},
		{"an all-whitespace value is forwarded, not emptied", "\t", "\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelID(tc.in); got != tc.want {
				t.Errorf("modelID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
