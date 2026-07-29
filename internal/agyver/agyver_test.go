package agyver

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
	for _, raw := range []string{"", "no digits here", "1.2", "v1"} {
		if got, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %v, want an error", raw, got)
		}
	}
}

// TestParseRanksCandidatesByContext is the whole contract in one table.
//
// Every row is an input that some previous shape of this function got wrong,
// and they are held together deliberately: the parser has been reworked twice
// and regressed both times because each fix was pinned by a test that covered
// only the input it was written for. A change that satisfies one row and breaks
// another must fail here rather than ship.
//
// History, for the reader deciding whether a fifth rework is safe:
//
//   - v1 took the first dotted triple anywhere, so a date, a path or an address
//     printed before the version won.
//   - v2 anchored the match to the START of a line, which made a real mid-line
//     version ineligible and handed the win to a log line beginning with a date.
//   - v3 anchored it to the WHOLE line, which fixed that and introduced the
//     mirror: a DECORATED version line is not a whole-line match either, so an
//     unrelated triple appearing earlier won instead.
func TestParseRanksCandidatesByContext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want Version
	}{
		{
			// v3's bug, mirrored from v2's. The version line is decorated with a
			// platform suffix, so it is not a whole-line match; the config path
			// printed above it must not win on being first.
			name: "decorated version line beats an earlier path triple",
			raw:  "config ~/.agy/2.0.0/cfg.toml\nagy 1.0.5 (linux/amd64)\n",
			want: Version{1, 0, 5},
		},
		{
			// v2's bug: a true version sitting mid-line, followed by a log line
			// that merely BEGINS with a numeric triple.
			name: "mid-line version beats a following date line",
			raw:  "warning: agy 1.0.5 is deprecated\n2026.07.29 starting\n",
			want: Version{1, 0, 5},
		},
		{
			name: "mid-line version beats a following address line",
			raw:  "using agy at /opt/agy: 1.0.5\n127.0.0.1 refused\n",
			want: Version{1, 0, 5},
		},
		{
			// v1's bug: a date printed before the version.
			name: "version on its own line beats a preceding date line",
			raw:  "2026.07.29 12:00:00 starting\n1.1.8\n",
			want: Version{1, 1, 8},
		},
		{
			name: "version on its own line beats a preceding path triple",
			raw:  "loading ~/.agy/9.9.9/state.db\n1.1.8\n",
			want: Version{1, 1, 8},
		},
		{
			name: "build suffix on its own line",
			raw:  "some noise\n1.2.0-preview.3\n",
			want: Version{1, 2, 0},
		},
		{
			// CombinedOutput merges stdout and stderr onto one fd, so a concurrent
			// stderr write can splice a warning onto the version line. A spliced
			// triple is still the only version present and must still be read.
			name: "spliced stderr still yields the version",
			raw:  "warning: cache stale1.1.8\n",
			want: Version{1, 1, 8},
		},
		{
			// An address is a fragment of a longer dotted chain, never a version,
			// so a bare triple elsewhere outranks it even without a marker word.
			name: "free-standing triple beats an address",
			raw:  "127.0.0.1 is the daemon\nbuilt from 1.1.9 today\n",
			want: Version{1, 1, 9},
		},
		{
			// A v-prefix belongs to the version, not to a word it was spliced onto.
			name: "v-prefixed triple is free-standing, not spliced",
			raw:  "/srv/2.2.2/lib loaded\nrunning v1.1.8 now\n",
			want: Version{1, 1, 8},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
// Every row of TestParseRanksCandidatesByContext that resolves to a pre-1.1.8
// version is a gate bypass if the parser picks the wrong candidate, so assert
// the consequence and not just the number.
func TestParseKeepsRefusingOldVersionsAmidNoise(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"config ~/.agy/2.0.0/cfg.toml\nagy 1.0.5 (linux/amd64)\n",
		"warning: agy 1.0.5 is deprecated\n2026.07.29 starting\n",
		"using agy at /opt/agy: 1.0.5\n127.0.0.1 refused\n",
	} {
		v, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", raw, err)
		}
		if v.AtLeast(Required) {
			t.Fatalf("Parse(%q) resolved to %v, which clears the %v floor; noisy output must not promote an old agy", raw, v, Required)
		}
	}
}

// A first component of four or more digits is a year, not an agy major:
// 2026.07.29 parses as major 2026 and clears every floor. Output whose only
// triples look like dates must refuse the binary rather than guess, because a
// wrong guess here is always the permissive direction.
func TestParseRejectsDateOnlyOutput(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"2026.07.29 12:00:00 starting\n",
		"log rotated at 2026.01.01 and 2025.12.31\n",
	} {
		if got, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %v, want an error rather than a date read as a version", raw, got)
		}
	}
}

// A line that is nothing but a triple is the output deliberately claiming that
// string as its version, so it is taken even with a four-digit first component:
// a future agy on calendar versioning must not be refused outright.
func TestParseAcceptsWholeLineCalendarVersion(t *testing.T) {
	t.Parallel()
	got, err := Parse("agy version 2026.1.0\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := (Version{2026, 1, 0}); got != want {
		t.Fatalf("Parse = %v, want %v", got, want)
	}
}

// An absurdly long run of digits overflows Atoi. Report it rather than
// silently reading a wrapped value as a plausible version, which is how a
// garbage line could otherwise clear the floor.
func TestParseReportsOverflow(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("9", 40)
	for _, raw := range []string{
		huge + ".0.0\n",
		"1." + huge + ".0\n",
		"1.0." + huge + "\n",
	} {
		if got, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %v, want an overflow error", raw, got)
		}
	}
}

func TestAtLeast(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	if got := (Version{1, 1, 8}).String(); got != "1.1.8" {
		t.Fatalf("String() = %q, want 1.1.8", got)
	}
	if got := Required.String(); got != "1.1.8" {
		t.Fatalf("Required = %q; update this test deliberately when the floor moves", got)
	}
}
