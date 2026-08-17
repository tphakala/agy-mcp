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

	// Shapes that must be a loud error, not a silent empty catalog. The last two
	// pin the framing checks: without the status/command validation each decodes to
	// a models list with err nil, and this case would go green.
	for _, tc := range []struct{ name, raw string }{
		{"malformed json", `{"status":"SUCCESS","command":`},
		{"status not SUCCESS", `{"status":"ERROR","command":{"name":"models","data":{"models":[{"id":"x"}]}}}`},
		{"wrong command", `{"status":"SUCCESS","command":{"name":"run","data":{"models":[{"id":"x"}]}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeModelsEnvelope([]byte(tc.raw)); err == nil {
				t.Fatalf("decodeModelsEnvelope(%s) succeeded, want an error", tc.name)
			}
		})
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
