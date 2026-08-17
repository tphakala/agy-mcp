// Package agyver parses and compares agy CLI versions, so agy-mcp can refuse a
// binary too old for the output format it depends on.
package agyver

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Required is the oldest agy that agy-mcp can drive. Three facts set the floor.
// 1.1.8 added --output-format (text, json, stream-json) to print mode, and the
// whole job pipeline reads the stream-json event stream, so an older agy cannot
// be used at all rather than degraded. 1.1.10 is the first release where the
// run-shaping flags agy-mcp forwards in headless -p (--model and --effort)
// actually take effect; on 1.1.8/1.1.9 they were silently ignored, so a lower
// floor would let a passing gate hide a no-op flag. 1.1.12 added machine-readable
// output to the `models` subcommand: `agy --output-format json models` returns a
// command envelope whose command.data.models is a list of {id,label} objects, and
// list_models decodes that typed field instead of tab-splitting the text rows.
// decodeModelsEnvelope refuses an envelope whose status is not SUCCESS, whose
// command is not `models`, or that omits the models array, so a shape change in
// any of those fails loudly rather than being read as an empty catalog; an agy
// without the envelope at all is simply too old to read. All three facts are
// from agy's own changelog (run
// `agy changelog`): the 1.1.12 line reads "machine-readable output to the models
// and agents subcommands".
var Required = Version{Major: 1, Minor: 1, Patch: 12}

// Version is a parsed agy version.
type Version struct {
	Major int
	Minor int
	Patch int
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// AtLeast reports whether v is o or newer.
func (v Version) AtLeast(o Version) bool {
	if v.Major != o.Major {
		return v.Major > o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor > o.Minor
	}
	return v.Patch >= o.Patch
}

// tier ranks how strongly a dotted triple's surroundings say it is the version.
// It is deliberately coarse: one level for "ruled out", one for "could be", and
// one for the single framing agy actually uses. Every finer distinction tried
// here has turned out to be an accept-too-much bug in one direction or the
// other.
//
// Parse and classify have between them been reworked repeatedly, and regressed
// each time. The record lives here, on the type the reworks kept redefining,
// because it is the argument for the shape they finally settled on:
//
//  1. "the first dotted triple anywhere" accepted dates, paths and addresses
//     printed before the version.
//  2. "a triple anchored to the START of a line" made a real mid-line version
//     ineligible while still matching a log line that merely begins with a
//     triple, so a date won outright.
//  3. "a triple that is the WHOLE line" fixed 2 and introduced the mirror: a
//     DECORATED version line is not a whole-line match either, so an unrelated
//     triple appearing earlier won.
//  4. Ranking a triple higher when an "agy" or "version" word preceded it fixed
//     3, and broke on the most ordinary thing a CLI prints: an upgrade notice.
//     "upgrade to agy 9.9.9" is marked by that rule and names a version HIGHER
//     than the binary printing it, so it outranked the real one.
//  5. Preferring the LOWEST reading among equals fixed 4 and broke the opposite
//     way: "agy 1.1.8 (built with 0.9.1 toolchain)" resolved to 0.9.1 and
//     REFUSED a working binary, which is worse than any bypass (see Parse).
//
// Every one of those came from trying to identify THE version by how it is
// written or by which number it is. Neither works: "agy 1.0.5" and "agy 9.9.9"
// are the same shape whether the line reports the version or advertises a newer
// one, and a sub-component version can be either higher or lower than agy's own.
// So a tier now answers only the question the text really can answer, which is
// whether a triple is part of something that is not a version at all.
type tier int

const (
	tierNone      tier = iota // no candidate at all; Parse reports an error
	tierEmbedded              // part of a path or a longer dotted chain: not a version
	tierCredible              // could be a version
	tierWholeLine             // the line holds nothing else: this is what agy itself prints
)

// wholeLineRE matches a line that IS a version: the triple alone, after nothing
// more than an optional "agy" and/or "version" word and an optional "v", with
// an optional build suffix. agy 1.1.8 prints a bare "1.1.8", but --version is
// not listed in --help, so its exact framing is not a contract; tolerating a
// future "agy version 1.2.0" or "1.2.0-preview.3" keeps a cosmetic change from
// becoming a hard refusal to run.
//
// A match earns tierWholeLine, the top tier, because this is the framing agy
// itself uses and a line holding nothing else is the strongest evidence the
// output can offer. It also stops the build suffix being rescanned:
// "1.2.0-preview.2.3.4" is one version, not two.
// The line must start at column 0. Leading whitespace was tolerated here and
// handed the top tier to the INDENTED continuation of an upgrade notice:
//
//	agy 1.0.5 (linux/amd64)
//	A new release of agy is available:
//	  9.9.9
//
// resolved to 9.9.9 and cleared the floor. agy prints its own version flush
// left, so indentation is evidence the line is a sub-item of something else,
// not the version. An indented triple is still read, just as an ordinary
// candidate rather than the strongest one in the output.
var wholeLineRE = regexp.MustCompile(`^(?:agy\s+)?(?:version\s+)?v?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]*)?\s*$`)

// tripleRE matches a dotted triple wherever it appears. On its own it accepts
// far too much (a path like ~/.agy/1.0.0/config, a date rendered 2026.07.29, an
// address such as 127.0.0.1), which is why every match is classified by its
// context rather than taken at face value.
var tripleRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// implausibleMajor is the value at or above which a first component is read as
// a year rather than an agy major, and the candidate is discarded.
//
// 2026.07.29 parses as major 2026 and clears every conceivable floor, so
// accepting it is always the unsafe direction. Refusing is loud rather than
// silent: with no candidate left the caller reports a parse failure and
// declines to run, instead of proceeding against a binary it could not
// identify. A future agy on calendar versioning would be refused this way, and
// that is the intended trade: a number this code cannot tell apart from a
// timestamp must not be the thing that clears the floor.
//
// It is compared by VALUE, not by digit count. Counting digits refused a
// zero-padded "0001.1.8", which is an ordinary version written oddly.
//
// A triple inside a longer dotted chain (127.0.0.1, or a four-component version
// string) is NOT discarded this way, and the asymmetry is deliberate. A
// four-digit first component cannot be an agy major, but a chain fragment can
// genuinely be the leading three components of a longer version string, so
// discarding it would turn a cosmetic version-format change into a refusal to
// run. It is demoted to tierEmbedded instead, where anything better beats it
// and it is only ever the answer of last resort.
const implausibleMajor = 1000

// candidate is one dotted triple found in the output, with the tier its
// surroundings earned it.
type candidate struct {
	tier tier
	m    []string // the triple's submatches, indexed as regexp returns them: full, major, minor, patch
}

// version converts a candidate, reporting ok=false for one that cannot be an
// agy version: an implausibly large major (see implausibleMajor), or a run of
// digits so long it overflows. Skipping such a candidate rather than failing
// the whole parse matters, because one unusable triple must not refuse a
// perfectly readable version printed on the next line.
func (c candidate) version() (Version, bool) {
	major, err := strconv.Atoi(c.m[1])
	if err != nil || major >= implausibleMajor {
		return Version{}, false
	}
	minor, err := strconv.Atoi(c.m[2])
	if err != nil {
		return Version{}, false
	}
	patch, err := strconv.Atoi(c.m[3])
	if err != nil {
		return Version{}, false
	}
	return Version{Major: major, Minor: minor, Patch: patch}, true
}

// Parse extracts a version from raw `agy --version` output.
//
// Every dotted triple is collected, those that cannot be a version are dropped,
// and of the rest the first at the best tier present wins.
//
// The tiers do the real work, and the top one is why: agy prints its version
// alone on a line, so a line holding nothing else is the strongest evidence the
// output can offer and it outranks any bare triple elsewhere. That is what
// keeps a diagnostic printed ABOVE the version from displacing it.
//
// Within a tier, FIRST, not lowest or highest, and that choice is the one place
// a maintainer should not "improve" without reading this. The two ways of being
// wrong are not symmetric, but not in the direction that looks obvious. Reading
// too HIGH bypasses the floor, and a bypass degrades LOUDLY: agy is a Go flag
// CLI, so a pre-1.1.8 binary handed an unknown --output-format exits 2 with
// usage text and the job reports failed with that text attached. Reading too
// LOW refuses a working binary, and the caller then declines to start any job
// at all. So the expensive failure is the false refusal, and a rule that
// resolves ambiguity by taking the smallest number manufactures exactly that:
// "agy 1.1.8 (built with 0.9.1 toolchain)" is an ordinary thing for a CLI to
// print, and reading it as 0.9.1 takes the whole server down. Sub-component
// versions are usually 0.x, so "prefer the smaller" and "prefer the later" both
// point the wrong way for them.
//
// Taking the first also happens to disarm the upgrade notice that broke an
// earlier rework, since such a line follows the version it is about.
//
// What remains unresolvable is a bare triple sharing the top tier with the real
// version and printed before it, on output where agy does NOT put its version
// on a line of its own. No rule tried here has separated those without breaking
// a case that matters more.
//
// It reports an error only when no usable triple appears at all, which includes
// output whose only triples are date-shaped.
func Parse(raw string) (Version, error) {
	var best Version
	bestTier := tierNone
	for line := range strings.Lines(raw) {
		for _, c := range candidates(line) {
			v, ok := c.version()
			// Strictly greater, so the first candidate at a given tier keeps it.
			if !ok || c.tier <= bestTier {
				continue
			}
			best, bestTier = v, c.tier
		}
	}
	if bestTier == tierNone {
		return Version{}, errors.New("no version number found")
	}
	return best, nil
}

// candidates returns every triple on one line, classified.
//
// A line that is nothing but a version yields exactly that one candidate: its
// build suffix ("-preview.2.3.4") would otherwise be rescanned and offer
// triples that are fragments of the same version rather than versions.
func candidates(line string) []candidate {
	if m := wholeLineRE.FindStringSubmatch(line); m != nil {
		return []candidate{{tier: tierWholeLine, m: m}}
	}
	var out []candidate
	for _, loc := range tripleRE.FindAllStringSubmatchIndex(line, -1) {
		out = append(out, candidate{
			tier: classify(line, loc[0], loc[1]),
			m:    submatches(line, loc),
		})
	}
	return out
}

// submatches materializes the full match and its three groups from the index
// pairs FindAllStringSubmatchIndex returns, so a candidate from this path is
// indexed exactly as one from FindStringSubmatch.
func submatches(line string, loc []int) []string {
	m := make([]string, 4)
	for i := range m {
		m[i] = line[loc[2*i]:loc[2*i+1]]
	}
	return m
}

// classify reports whether a triple's surroundings make it part of something
// that is not a version.
//
// There are exactly two such things, and the list is short on purpose. A triple
// spliced onto a word ("agy1.0.5") is deliberately NOT one of them: demoting it
// meant a real version lost to unrelated noise elsewhere in the output, and the
// splice is something this code causes itself, since CombinedOutput merges
// stdout and stderr onto one fd and a concurrent stderr write lands mid-line.
func classify(line string, start, end int) tier {
	before, after := line[:start], line[end:]
	// Part of a longer dotted chain: 127.0.0.1 (an address) or 1.2.3.4, where
	// the triple is a fragment of something larger.
	//
	// The neighbouring character has to be a DIGIT, not merely a dot. Testing
	// for a bare dot read the full stop in "You are running agy 1.0.5." as a
	// chain and demoted the one real version on the line.
	if endsWithDigitDot(before) || startsWithDotDigit(after) {
		return tierEmbedded
	}
	// A path component: /opt/agy/2.0.0/bin, ~/.agy/2.0.0/cfg.toml, or
	// C:\agy\2.0.0\bin. This is the demotion issue #98 turned on, so it is the
	// one worth keeping.
	if inPathToken(trimVPrefix(before)) {
		return tierEmbedded
	}
	return tierCredible
}

// inPathToken reports whether the text immediately before a triple makes it a
// component of a path rather than a version.
//
// The test is a separator directly before the triple AND another one earlier in
// the same token, and both halves are load bearing:
//
//   - Requiring only the separator directly before swept up the "prog/x.y.z"
//     framing curl, wget and git all print. "agy/1.1.8" is a version with a
//     slash in front of it, not a directory, and demoting it let an unrelated
//     triple further down the output win.
//   - Requiring a separator on both SIDES instead (so that only a middle
//     component counts) looked like the fix and was worse: a versioned
//     directory named at the END of a line has nothing after it, so an ordinary
//     "could not remove ~/.cache/agy/0.9.1" was read as a version and refused a
//     working binary.
//
// A second separator earlier in the token is what actually separates the two:
// a path names at least one directory before the component, where "agy/1.1.8"
// names none.
func inPathToken(before string) bool {
	// Narrow to the token the triple sits in, so a separator in an unrelated
	// earlier word cannot vouch for it.
	if i := strings.LastIndexAny(before, " \t\"'`(<[{,=:"); i >= 0 {
		before = before[i+1:]
	}
	if !endsWithPathSep(before) {
		return false
	}
	return strings.ContainsAny(before[:len(before)-1], `/\`)
}

// endsWithDigitDot and startsWithDotDigit report whether the triple continues a
// dotted number on either side.
func endsWithDigitDot(s string) bool {
	return len(s) >= 2 && s[len(s)-1] == '.' && isDigit(s[len(s)-2])
}

func startsWithDotDigit(s string) bool {
	return len(s) >= 2 && s[0] == '.' && isDigit(s[1])
}

// trimVPrefix drops a "v" immediately preceding the triple, so that the v in a
// path component such as "/opt/agy/v1.0.5/bin" does not hide the separator.
func trimVPrefix(before string) string {
	if before == "" {
		return before
	}
	if last := before[len(before)-1]; last == 'v' || last == 'V' {
		return before[:len(before)-1]
	}
	return before
}

func endsWithPathSep(s string) bool {
	return strings.HasSuffix(s, "/") || strings.HasSuffix(s, `\`)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
