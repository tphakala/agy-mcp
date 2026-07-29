// Package agyver parses and compares agy CLI versions, so agy-mcp can refuse a
// binary too old for the output format it depends on.
package agyver

import (
	"fmt"
	"regexp"
	"strconv"
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

// versionRE matches the first dotted triple in a string. agy 1.1.8 prints a
// bare "1.1.8" for --version, but that flag is not listed in --help, so its
// exact framing is not a contract. Matching the first triple anywhere in the
// output keeps a future "agy version 1.2.0" or a build-suffixed
// "1.2.0-preview" parsing correctly instead of turning a cosmetic change into a
// hard refusal to run.
var versionRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// Parse extracts a version from raw `agy --version` output. It reports an error
// only when no dotted triple appears at all.
func Parse(raw string) (Version, error) {
	m := versionRE.FindStringSubmatch(raw)
	if m == nil {
		return Version{}, fmt.Errorf("no version number found")
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
