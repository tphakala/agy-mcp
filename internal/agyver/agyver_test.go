package agyver

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Version
	}{
		// What agy 1.1.8 actually prints for --version.
		{"bare", "1.1.8\n", Version{1, 1, 8}},
		// The flag is undocumented, so tolerate a future framing rather than
		// turning a cosmetic change into a refusal to run.
		{"prefixed", "agy version 1.2.0", Version{1, 2, 0}},
		{"suffixed", "1.2.0-preview.3", Version{1, 2, 0}},
		{"padded", "  2.0.10  \n", Version{2, 0, 10}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseRejectsNonVersions(t *testing.T) {
	for _, raw := range []string{"", "no digits here", "1.2", "v1"} {
		if got, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", raw, got)
		}
	}
}

func TestAtLeast(t *testing.T) {
	cases := []struct {
		v, min Version
		want   bool
	}{
		{Version{1, 1, 8}, Required, true},  // exactly the minimum
		{Version{1, 1, 9}, Required, true},  // patch ahead
		{Version{1, 2, 0}, Required, true},  // minor ahead
		{Version{2, 0, 0}, Required, true},  // major ahead
		{Version{1, 1, 7}, Required, false}, // the release just before
		{Version{1, 0, 99}, Required, false},
		{Version{0, 9, 9}, Required, false},
	}
	for _, tc := range cases {
		if got := tc.v.AtLeast(tc.min); got != tc.want {
			t.Errorf("%v.AtLeast(%v) = %v, want %v", tc.v, tc.min, got, tc.want)
		}
	}
}

func TestString(t *testing.T) {
	if got := (Version{1, 1, 8}).String(); got != "1.1.8" {
		t.Fatalf("String() = %q, want 1.1.8", got)
	}
	if got := Required.String(); got != "1.1.8" {
		t.Fatalf("Required = %q; update this test deliberately when the floor moves", got)
	}
}

// A true version sitting mid-line, followed by a log line that merely BEGINS
// with a numeric triple. Anchoring only the start of the line made the real
// version ineligible and let the date win, accepting an agy that the floor
// should have refused.
func TestParsePrefersRealVersionOverLeadingNumericNoise(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want Version
	}{
		{"date line after a mid-line version", "warning: agy 1.0.5 is deprecated\n2026.07.29 starting\n", Version{1, 0, 5}},
		{"ip line after a mid-line version", "using agy at /opt/agy: 1.0.5\n127.0.0.1 refused\n", Version{1, 0, 5}},
		{"version on its own line still wins", "2026.07.29 12:00:00 starting\n1.1.8\n", Version{1, 1, 8}},
		{"build suffix on its own line", "some noise\n1.2.0-preview.3\n", Version{1, 2, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// The floor must still refuse what it refused before the parser was reworked.
func TestParseKeepsRefusingOldVersionsAmidNoise(t *testing.T) {
	v, err := Parse("warning: agy 1.0.5 is deprecated\n2026.07.29 starting\n")
	if err != nil {
		t.Fatal(err)
	}
	if v.AtLeast(Required) {
		t.Fatalf("Parse resolved to %v, which clears the %v floor; noisy output must not promote an old agy", v, Required)
	}
}
