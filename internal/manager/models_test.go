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

// TestParseModels pins the split of each `agy models` row into its id and label
// columns. The raw text is the shape agy 1.1.11 prints: "<id>\t<label>" per row.
// Keeping the whole row as one "model name" is what issue #135 was, since the
// row is not a value agy accepts as --model at all.
func TestParseModels(t *testing.T) {
	raw := "gemini-3.5-flash-medium\tGemini 3.5 Flash (Medium)\n" +
		"gemini-3.1-pro-high\tGemini 3.1 Pro (High)\n" +
		"\n" +
		"   \n" + // whitespace-only rows are skipped like blank ones
		"  claude-opus-4-6-thinking \t  Claude Opus 4.6 (Thinking)  \n" + // padding is trimmed off both columns
		"gpt-oss-120b-medium\tGPT-OSS 120B\t(Medium)\n" // only the FIRST tab cuts
	want := []Model{
		{ID: "gemini-3.5-flash-medium", Label: "Gemini 3.5 Flash (Medium)"},
		{ID: "gemini-3.1-pro-high", Label: "Gemini 3.1 Pro (High)"},
		{ID: "claude-opus-4-6-thinking", Label: "Claude Opus 4.6 (Thinking)"},
		{ID: "gpt-oss-120b-medium", Label: "GPT-OSS 120B\t(Medium)"},
	}
	if got := parseModels(raw); !slices.Equal(got, want) {
		t.Fatalf("parseModels() = %#v, want %#v", got, want)
	}
}

// TestParseModelsWithoutLabelColumn: a row with no tab is an id with an empty
// label, not a dropped row, so output carrying no label column still yields
// usable model values.
func TestParseModelsWithoutLabelColumn(t *testing.T) {
	want := []Model{{ID: "gemini-3.1-pro-high"}, {ID: "claude-sonnet-4-6"}}
	if got := parseModels("gemini-3.1-pro-high\nclaude-sonnet-4-6\n"); !slices.Equal(got, want) {
		t.Fatalf("parseModels() = %#v, want %#v", got, want)
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
		// Trimming must match parseModels, or the id list_models reports for a row
		// and the id that reaches --model for that same row would differ.
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
