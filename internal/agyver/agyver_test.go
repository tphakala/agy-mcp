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
