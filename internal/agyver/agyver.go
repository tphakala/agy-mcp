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

// Required is the oldest agy that agy-mcp can drive. 1.1.8 added
// --output-format (text, json, stream-json) to print mode; the whole job
// pipeline reads the stream-json event stream, so an older agy cannot be used
// at all rather than degraded.
var Required = Version{Major: 1, Minor: 1, Patch: 8}

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

// tier ranks how strongly a dotted triple's surroundings say it is really a
// version rather than a date, a path component or an address. Parse keeps the
// highest-tier candidate in the whole output, which is what lets a decorated
// version line beat unrelated numeric noise appearing before it.
//
// Ranking beats matching here because this function has now been reworked
// twice and regressed both times, in opposite directions: first "the first
// triple anywhere", which accepted dates, paths and IPs; then "a triple
// anchored to the line", which made a real mid-line version ineligible and let
// a date win. Both failures come from a single yes/no test having to serve as
// both the recognizer and the tie-breaker. Splitting the two is what stops the
// next input from trading one bug for another.
type tier int

const (
	tierNone      tier = iota
	tierEmbedded       // inside a path, spliced onto a word, or part of a longer dotted chain
	tierFree           // free-standing: neither of the above, but nothing vouches for it either
	tierMarked         // an "agy" or "version" word immediately precedes it
	tierWholeLine      // the line is nothing but this version
)

// wholeLineRE matches a line that IS a version: the triple alone, after nothing
// more than an optional "agy" and/or "version" word and an optional "v", with
// an optional build suffix. agy 1.1.8 prints a bare "1.1.8", but --version is
// not listed in --help, so its exact framing is not a contract; tolerating a
// future "agy version 1.2.0" or "1.2.0-preview.3" keeps a cosmetic change from
// becoming a hard refusal to run.
var wholeLineRE = regexp.MustCompile(`^\s*(?:agy\s+)?(?:version\s+)?v?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]*)?\s*$`)

// tripleRE matches a dotted triple wherever it appears. On its own it accepts
// far too much (a path like ~/.agy/1.0.0/config, a date rendered 2026.07.29, an
// address such as 127.0.0.1), which is why every match is scored by its context
// rather than taken at face value.
var tripleRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// markerRE matches the "agy" or "version" word ending the text immediately
// before a triple. It is what separates "agy 1.0.5 (linux/amd64)" from a triple
// that merely shares a line with the word, so a path such as "~/.agy/2.0.0/cfg"
// is not promoted by the "agy" inside it.
var markerRE = regexp.MustCompile(`(?i)\b(?:agy|version)\s+$`)

// dateLikeMajor is the digit count above which a first component is read as a
// year rather than an agy major. 2026.07.29 parses as major 2026 and clears
// every conceivable floor, so accepting it is always the unsafe direction;
// rejecting it is loud, since the caller reports the parse failure and refuses
// to run rather than silently proceeding against an unknown binary.
const dateLikeMajor = 4

// candidate is one dotted triple found in the output, with the tier its
// surroundings earned it.
type candidate struct {
	tier tier
	m    []string // the triple's submatches: full, major, minor, patch
}

// Parse extracts a version from raw `agy --version` output.
//
// Every dotted triple in the output is collected and scored by its context; the
// highest-scoring one wins, and the earliest wins a tie. It reports an error
// when no plausible triple appears at all, which includes output whose only
// triples look like dates: an unreadable version must refuse the binary rather
// than guess a number that would clear the floor.
func Parse(raw string) (Version, error) {
	var best candidate
	for line := range strings.Lines(raw) {
		for _, c := range candidates(line) {
			// Strictly greater, so the earliest of equally-ranked candidates wins.
			if c.tier > best.tier {
				best = c
			}
		}
	}
	if best.tier == tierNone {
		return Version{}, errors.New("no version number found")
	}
	// Each group is one or more digits, so the only Atoi failure left is an
	// overflow from an absurdly long run of digits. Report that rather than
	// silently reading a wrapped value as a plausible version.
	major, err := strconv.Atoi(best.m[1])
	if err != nil {
		return Version{}, fmt.Errorf("major version %q: %w", best.m[1], err)
	}
	minor, err := strconv.Atoi(best.m[2])
	if err != nil {
		return Version{}, fmt.Errorf("minor version %q: %w", best.m[2], err)
	}
	patch, err := strconv.Atoi(best.m[3])
	if err != nil {
		return Version{}, fmt.Errorf("patch version %q: %w", best.m[3], err)
	}
	return Version{Major: major, Minor: minor, Patch: patch}, nil
}

// candidates returns every usable version candidate on one line, scored.
//
// A line that is nothing but a version yields exactly that one candidate: the
// whole-line reading already dominates anything else the line could offer, and
// a build suffix like "-preview.3" would otherwise be rescanned for triples.
func candidates(line string) []candidate {
	if m := wholeLineRE.FindStringSubmatch(line); m != nil {
		return []candidate{{tier: tierWholeLine, m: m}}
	}
	var out []candidate
	for _, loc := range tripleRE.FindAllStringSubmatchIndex(line, -1) {
		m := submatches(line, loc)
		if len(m[1]) >= dateLikeMajor {
			// A year, not a major. Only a whole-line match (handled above) is taken
			// as a deliberate claim, and this line is not one.
			continue
		}
		out = append(out, candidate{tier: classify(line, loc[0], loc[1]), m: m})
	}
	return out
}

// submatches materializes the full match and its three groups from the index
// pairs FindAllStringSubmatchIndex returns, so the rest of the scoring works on
// strings exactly as FindStringSubmatch would hand them over.
func submatches(line string, loc []int) []string {
	m := make([]string, 4)
	for i := range m {
		m[i] = line[loc[2*i]:loc[2*i+1]]
	}
	return m
}

// classify scores one triple by what surrounds it on its line.
func classify(line string, start, end int) tier {
	before, after := line[:start], line[end:]
	// Part of a longer dotted chain: 127.0.0.1 (an address) or 1.2.3.4. The
	// triple is a fragment of something that is not a version, whatever else the
	// line says, so no marker can promote it.
	if strings.HasSuffix(before, ".") || strings.HasPrefix(after, ".") {
		return tierEmbedded
	}
	before = trimVPrefix(before)
	if markerRE.MatchString(before) {
		return tierMarked
	}
	// A path component (/opt/agy/2.0.0/bin) or a triple spliced onto a word.
	// CombinedOutput merges stdout and stderr onto one fd, so a concurrent
	// stderr write really can splice a warning onto the version line; such a
	// triple is still readable, just not vouched for.
	if endsWithPathSep(before) || strings.HasPrefix(after, "/") || strings.HasPrefix(after, `\`) {
		return tierEmbedded
	}
	if before != "" && isWordByte(before[len(before)-1]) {
		return tierEmbedded
	}
	return tierFree
}

// trimVPrefix drops a "v" immediately preceding the triple when the v itself
// starts a word ("v1.2.0"), so the v reads as part of the version rather than
// as a word the triple was spliced onto.
func trimVPrefix(before string) string {
	if before == "" {
		return before
	}
	if last := before[len(before)-1]; last != 'v' && last != 'V' {
		return before
	}
	rest := before[:len(before)-1]
	if rest != "" && isWordByte(rest[len(rest)-1]) {
		return before // "rev1.2.0": the v belongs to the preceding word
	}
	return rest
}

func endsWithPathSep(s string) bool {
	return strings.HasSuffix(s, "/") || strings.HasSuffix(s, `\`)
}

// isWordByte reports whether c is an ASCII identifier byte. Only ASCII matters:
// it is used to decide whether a triple abuts a word, and a multi-byte rune's
// continuation bytes are all >= 0x80, so they fall through as non-word, which
// is the conservative answer.
func isWordByte(c byte) bool {
	return c == '_' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}
