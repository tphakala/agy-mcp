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

// leadingVersionRE matches a line that IS a version: the triple at the start,
// after nothing more than an optional "agy" and/or "version" word and an
// optional "v". agy 1.1.8 prints a bare "1.1.8", but that flag is not listed in
// --help, so its exact framing is not a contract; this keeps a future "agy
// version 1.2.0" or a build-suffixed "1.2.0-preview" parsing rather than
// turning a cosmetic change into a hard refusal to run.
// The line must contain NOTHING BUT the version, apart from that prefix and an
// optional build suffix. Anchoring only the start was worse than no anchoring at
// all: it made a true version sitting mid-line ineligible while still matching
// any line that merely BEGINS with a numeric triple, so "warning: agy 1.0.5 is
// deprecated" followed by a log line starting "2026.07.29" resolved to 2026.7.29
// and cleared a floor that 1.0.5 correctly failed. Requiring the whole line
// leaves such output to the fallback, which picks the real 1.0.5.
var leadingVersionRE = regexp.MustCompile(`^\s*(?:agy\s+)?(?:version\s+)?v?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]*)?\s*$`)

// anywhereVersionRE is the last-resort fallback: any dotted triple at all.
//
// It is tried only after every line has failed the anchored match, because on
// its own it accepts far too much. Callers feed it merged stdout and stderr, so
// an unrelated leading triple wins outright: a path like ~/.agy/1.0.0/config, a
// date rendered 2026.07.29 (which would parse as major 2026 and clear any
// floor), or an IP such as 127.0.0.1. Preferring an anchored match keeps the
// tolerance without letting a stray warning line decide the version.
var anywhereVersionRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// Parse extracts a version from raw `agy --version` output. It reports an error
// only when no dotted triple appears at all.
func Parse(raw string) (Version, error) {
	var m []string
	for line := range strings.Lines(raw) {
		if m = leadingVersionRE.FindStringSubmatch(line); m != nil {
			break
		}
	}
	if m == nil {
		m = anywhereVersionRE.FindStringSubmatch(raw)
	}
	if m == nil {
		return Version{}, errors.New("no version number found")
	}
	// Each group is one or more digits, so the only Atoi failure left is an
	// overflow from an absurdly long run of digits. Report that rather than
	// silently reading a wrapped value as a plausible version.
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return Version{}, fmt.Errorf("major version %q: %w", m[1], err)
	}
	minor, err := strconv.Atoi(m[2])
	if err != nil {
		return Version{}, fmt.Errorf("minor version %q: %w", m[2], err)
	}
	patch, err := strconv.Atoi(m[3])
	if err != nil {
		return Version{}, fmt.Errorf("patch version %q: %w", m[3], err)
	}
	return Version{Major: major, Minor: minor, Patch: patch}, nil
}
