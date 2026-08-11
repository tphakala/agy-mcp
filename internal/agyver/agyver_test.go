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

// parseCase is one raw `agy --version` output and the version it must resolve
// to. TestParseResolvesNoisyOutput runs every case, and
// TestParseNeverPromotesPastTheFloor derives its inputs from the same slice, so
// the two cannot drift.
type parseCase struct {
	name string
	raw  string
	want Version
}

// parseCases is the whole contract in one table.
//
// Every row is an input that some previous shape of the parser got wrong, and
// they are held together deliberately: it has been reworked repeatedly and
// regressed each time, because each fix was pinned by a test covering only the
// input it was written for. A change that satisfies one row and breaks another
// must fail here rather than ship.
//
// The numbered history of those reworks lives on the tier type in agyver.go,
// next to the design it argues for; the section comments below name which one
// each group of rows came from rather than restating it, so the two cannot
// drift apart.
var parseCases = []parseCase{
	{
		// v3's bug, mirrored from v2's. The version line is decorated with a
		// platform suffix, so it is not a whole-line match; the config path
		// printed above it must not win on being first.
		name: "decorated version line beats an earlier path triple",
		raw:  "config ~/.agy/2.0.0/cfg.toml\nagy 1.0.5 (linux/amd64)\n",
		want: Version{1, 0, 5},
	}, {
		// v2's bug: a true version sitting mid-line, followed by a log line
		// that merely BEGINS with a numeric triple.
		name: "mid-line version beats a following date line",
		raw:  "warning: agy 1.0.5 is deprecated\n2026.07.29 starting\n",
		want: Version{1, 0, 5},
	}, {
		name: "mid-line version beats a following address line",
		raw:  "using agy at /opt/agy: 1.0.5\n127.0.0.1 refused\n",
		want: Version{1, 0, 5},
	}, {
		// v1's bug: a date printed before the version.
		name: "version on its own line beats a preceding date line",
		raw:  "2026.07.29 12:00:00 starting\n1.1.8\n",
		want: Version{1, 1, 8},
	}, {
		name: "version on its own line beats a preceding path triple",
		raw:  "loading ~/.agy/9.9.9/state.db\n1.1.8\n",
		want: Version{1, 1, 8},
	}, {
		name: "build suffix on its own line",
		raw:  "some noise\n1.2.0-preview.3\n",
		want: Version{1, 2, 0},
	}, {
		// A whole-line match must not be rescanned: the triple inside the build
		// suffix is a fragment of this same version, not a second candidate.
		name: "build suffix containing its own triple",
		raw:  "1.2.0-preview.2.3.4\n",
		want: Version{1, 2, 0},
	}, {
		// CombinedOutput merges stdout and stderr onto one fd, so a concurrent
		// stderr write can splice a warning onto the version line. A spliced
		// triple is still the only version present and must still be read.
		name: "spliced stderr still yields the version",
		raw:  "warning: cache stale1.1.8\n",
		want: Version{1, 1, 8},
	}, {
		// An address is a fragment of a longer dotted chain, so a free-standing
		// triple outranks it.
		name: "free-standing triple beats an address",
		raw:  "127.0.0.1 is the daemon\nbuilt from 1.1.9 today\n",
		want: Version{1, 1, 9},
	}, {
		// A v-prefix belongs to the version, not to a word it was spliced onto.
		name: "v-prefixed triple is free-standing, not spliced",
		raw:  "/srv/2.2.2/lib loaded\nrunning v1.1.8 now\n",
		want: Version{1, 1, 8},
	},

	// --- v4's bug: an upgrade notice names a version higher than the installed
	// one, in exactly the shape a real version line has. Every row below
	// resolved to the ADVERTISED version under v4 and cleared the floor.
	{
		name: "upgrade notice after the version",
		raw:  "1.0.5 (deprecated build)\nupgrade to agy 9.9.9\n",
		want: Version{1, 0, 5},
	}, {
		name: "availability notice naming agy",
		raw:  "revision 1.0.5\nagy 9.9.9 is available, please upgrade\n",
		want: Version{1, 0, 5},
	}, {
		name: "a required runtime version is not agy's version",
		raw:  "built from 1.0.5\nrequires runtime version 9.9.9\n",
		want: Version{1, 0, 5},
	}, {
		name: "a config schema version is not agy's version",
		raw:  "binary: 1.0.5\nconfig schema version 3.0.0 detected\n",
		want: Version{1, 0, 5},
	},

	// --- v5's bug: resolving ambiguity by taking the LOWEST reading refused a
	// working binary, which is the expensive direction (see Parse). A CLI
	// printing a sub-component version alongside its own is entirely ordinary.
	{
		name: "a lower plugin version does not displace agy's own",
		raw:  "agy version 1.1.8\nplugin api 0.9.0\n",
		want: Version{1, 1, 8},
	}, {
		name: "a lower protocol version does not displace agy's own",
		raw:  "agy 1.1.8 (linux)\nmcp protocol 0.1.0\n",
		want: Version{1, 1, 8},
	}, {
		name: "a lower toolchain version on the same line",
		raw:  "agy 1.1.8 (built with 0.9.1 toolchain)\n",
		want: Version{1, 1, 8},
	}, {
		// A full stop is not a dotted chain. Reading it as one demoted the only
		// real version on the line and handed the answer to unrelated noise.
		name: "a sentence-final period is not a longer chain",
		raw:  "You are running agy 1.0.5.\nfound plugin 3.0.0\n",
		want: Version{1, 0, 5},
	}, {
		// The splice this code causes itself, by merging stdout and stderr onto
		// one fd. Demoting it let a later unrelated triple win.
		name: "a spliced version still beats later noise",
		raw:  "agy1.0.5\nplugin 3.0.0\n",
		want: Version{1, 0, 5},
	},

	// --- the mirror of v5's bug: a sub-component version printed BEFORE the
	// real one. Taking the first candidate fixed the trailing case and left
	// this one, which is the same false refusal. A line holding nothing but a
	// version outranks a bare triple anywhere else, which is what settles it.
	{
		name: "a lower protocol version printed above a bare version line",
		raw:  "agy: warning: mcp protocol 0.1.0 unsupported\nagy 1.1.8\n",
		want: Version{1, 1, 8},
	}, {
		name: "a deprecated schema version printed above the version",
		raw:  "warning: config schema 0.2.0 is deprecated\n1.1.8\n",
		want: Version{1, 1, 8},
	}, {
		name: "a lower plugin version printed above the version",
		raw:  "plugin api 0.9.0 loaded\nagy version 1.1.8\n",
		want: Version{1, 1, 8},
	}, {
		// The prog/x.y.z framing curl, wget and git all print. Requiring a
		// separator on only one side read it as a path component and demoted
		// the real version below unrelated noise further down.
		name: "prog/version framing is not a path component",
		raw:  "agy/1.1.8\nplugin api 0.9.0\n",
		want: Version{1, 1, 8},
	}, {
		name: "a trailing platform suffix is not a path component",
		raw:  "1.1.8/linux-amd64\nplugin api 0.9.0\n",
		want: Version{1, 1, 8},
	},

	// --- v6's bug, both directions. Requiring a separator on BOTH sides so
	// that only a middle component counted looked like the fix for the two rows
	// above, and broke every versioned directory named at the END of a line,
	// which is the ordinary way a diagnostic mentions one. What separates the
	// two is a second separator earlier in the same token: a path names a
	// directory before the component, where "agy/1.1.8" names none.
	{
		name: "a versioned cache directory at end of line is not the version",
		raw:  "agy: warning: could not remove ~/.cache/agy/0.9.1\nagy 1.1.8 (linux/amd64)\n",
		want: Version{1, 1, 8},
	}, {
		name: "a versioned plugin directory at end of line is not the version",
		raw:  "warning: no plugins in /usr/lib/agy/1.0.5\nagy 1.1.8 (linux/amd64)\n",
		want: Version{1, 1, 8},
	}, {
		// The permanent fixture above with one component removed. It used to
		// flip the answer, which is how thin the both-sides rule was.
		name: "a two-component path is still a path",
		raw:  "config ~/.agy/2.0.0\nagy 1.0.5 (linux/amd64)\n",
		want: Version{1, 0, 5},
	}, {
		name: "a quoted path at end of line is not the version",
		raw:  "cannot open \"/opt/agy/2.0.0\"\nagy 1.0.5 (linux/amd64)\n",
		want: Version{1, 0, 5},
	}, {
		name: "a path followed by a colon is not the version",
		raw:  "/opt/agy/2.0.0: permission denied\nagy 1.0.5 (linux/amd64)\n",
		want: Version{1, 0, 5},
	}, {
		name: "a versioned URL is not the version",
		raw:  "see https://agy.dev/docs/9.9.9\nagy 1.0.5 (linux/amd64)\n",
		want: Version{1, 0, 5},
	}, {
		name: "a truncated Windows path is still a path",
		raw:  "C:\\tools\\agy\\9.9.9 loaded\nagy 1.1.8 (linux/amd64)\n",
		want: Version{1, 1, 8},
	},

	// --- v6's other bug: an upgrade notice that puts the new version on its
	// own INDENTED line took the whole-line tier and beat the decorated real
	// version, whichever came first. agy prints its version flush left, so
	// indentation says the line is a sub-item of something else.
	{
		name: "an indented release notice does not outrank the version",
		raw:  "agy 1.0.5 (linux/amd64)\nA new release of agy is available:\n  9.9.9\n",
		want: Version{1, 0, 5},
	}, {
		name: "an indented v-prefixed release notice",
		raw:  "agy 1.0.5 (linux/amd64)\nlatest release:\n  v9.9.9\n",
		want: Version{1, 0, 5},
	}, {
		name: "an indented download filename",
		raw:  "agy 1.0.5 (linux/amd64)\ndownload:\n  9.9.9-linux-amd64.tar.gz\n",
		want: Version{1, 0, 5},
	}, {
		// implausibleMajor only catches four-digit years, so a two-digit build
		// stamp alone on a line would otherwise sit in the top tier.
		name: "an indented two-digit-year build stamp",
		raw:  "agy 1.0.5 (linux/amd64)\nbuild date:\n  26.07.29\n",
		want: Version{1, 0, 5},
	}, {
		name: "an indented lower protocol version does not refuse the binary",
		raw:  "agy 1.1.8 (linux/amd64)\nmcp protocol:\n  0.1.0\n",
		want: Version{1, 1, 8},
	}, {
		name: "a quoted version",
		raw:  "agy version \"1.2.0\" (linux/amd64)\n",
		want: Version{1, 2, 0},
	}, {
		// A zero-padded major is an ordinary version written oddly. Counting
		// digits rather than comparing the value refused it outright.
		name: "a zero-padded major is not a date",
		raw:  "agy 0001.1.8 (linux)\n",
		want: Version{1, 1, 8},
	}, {
		// One unreadable triple must not refuse a readable version beside it.
		name: "an overflowing triple does not poison the parse",
		raw:  strings.Repeat("9", 40) + ".0.0\nagy 1.1.8 (linux)\n",
		want: Version{1, 1, 8},
	}, {
		// CombinedOutput on Windows delivers CRLF.
		name: "CRLF line endings",
		raw:  "config ~/.agy/2.0.0/cfg.toml\r\nagy 1.0.5 (linux/amd64)\r\n",
		want: Version{1, 0, 5},
	}, {
		name: "CRLF whole-line version",
		raw:  "some noise\r\n1.1.8\r\n",
		want: Version{1, 1, 8},
	}, {
		name: "a Windows path component is not a version",
		raw:  `C:\tools\agy\9.9.9\bin loaded` + "\nagy 1.1.8\n",
		want: Version{1, 1, 8},
	}, {
		name: "no trailing newline",
		raw:  "agy 1.1.8 (linux/amd64)",
		want: Version{1, 1, 8},
	},
}

func TestParseResolvesNoisyOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range parseCases {
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

// The number is only half of it: every case above that resolves to a sub-floor
// version (below Required) is a gate bypass if the parser picks the wrong
// candidate, so assert the consequence too. The inputs are DERIVED from
// parseCases rather than copied, so a case added there is covered here
// automatically.
func TestParseNeverPromotesPastTheFloor(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, tc := range parseCases {
		if tc.want.AtLeast(Required) {
			continue
		}
		checked++
		v, err := Parse(tc.raw)
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.raw, err)
			continue
		}
		if v.AtLeast(Required) {
			t.Errorf("Parse(%q) resolved to %v, which clears the %v floor; noisy output must not promote an old agy", tc.raw, v, Required)
		}
	}
	if checked == 0 {
		t.Fatal("no sub-floor case in parseCases, so this test asserts nothing")
	}
}

// A first component at or above implausibleMajor is a year, not an agy major:
// 2026.07.29 parses as major 2026 and clears every floor. Output whose only
// triples look like dates must refuse the binary rather than guess, because a
// wrong guess here is always the permissive direction. The rule applies to a
// whole line too: exempting one put the least trustworthy reading at the top of
// the ranking.
func TestParseRejectsDateOnlyOutput(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"2026.07.29 12:00:00 starting\n",
		"log rotated at 2026.01.01 and 2025.12.31\n",
		"2026.07.29\n",
		"agy version 2026.1.0\n",
	} {
		if got, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) = %v, want an error rather than a date read as a version", raw, got)
		}
	}
}

// An absurdly long run of digits overflows Atoi. Such a candidate is skipped,
// so output containing nothing else has no version at all rather than a wrapped
// value that would clear the floor.
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

// trimVPrefix exists so a v-prefixed PATH component is still recognized as a
// path: without it the "v" hides the separator and the triple reads as an
// ordinary version. Nothing else covers that branch.
func TestParseSeesThroughAVPrefixedPathComponent(t *testing.T) {
	t.Parallel()
	got, err := Parse("loading /opt/agy/v9.9.9/state.db\nagy 1.1.8 (linux)\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := (Version{1, 1, 8}); got != want {
		t.Fatalf("Parse = %v, want %v: a v-prefixed path component is still a path", got, want)
	}
}

// Parse must round-trip its own rendering, whatever else it does.
func FuzzParseRoundTrip(f *testing.F) {
	for _, tc := range parseCases {
		f.Add(tc.raw)
	}
	f.Add("1.1.8\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, raw string) {
		v, err := Parse(raw)
		if err != nil {
			return
		}
		if v.Major < 0 || v.Minor < 0 || v.Patch < 0 {
			t.Fatalf("Parse(%q) = %v, which has a negative component", raw, v)
		}
		if v.Major >= implausibleMajor {
			t.Fatalf("Parse(%q) = %v, whose major is date-shaped and must have been discarded", raw, v)
		}
		got, err := Parse(v.String())
		if err != nil {
			t.Fatalf("Parse(%q) = %v, but re-parsing %q failed: %v", raw, v, v.String(), err)
		}
		if got != v {
			t.Fatalf("Parse(%q) = %v, but round-tripping through %q gave %v", raw, v, v.String(), got)
		}
	})
}

func TestAtLeast(t *testing.T) {
	t.Parallel()
	// AtLeast is pure version ordering, so pin a fixed reference rather than the
	// production floor (Required): this test must not move when the floor moves.
	// The floor boundary itself stays covered by the manager version tests and by
	// TestParseNeverPromotesPastTheFloor.
	ref := Version{1, 1, 8}
	cases := []struct {
		v, min Version
		want   bool
	}{
		{Version{1, 1, 8}, ref, true},  // exactly the reference
		{Version{1, 1, 9}, ref, true},  // patch ahead
		{Version{1, 2, 0}, ref, true},  // minor ahead
		{Version{2, 0, 0}, ref, true},  // major ahead
		{Version{1, 1, 7}, ref, false}, // the release just before
		{Version{1, 0, 99}, ref, false},
		{Version{0, 9, 9}, ref, false},
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
	if got := Required.String(); got != "1.1.11" {
		t.Fatalf("Required = %q; update this test deliberately when the floor moves", got)
	}
}
