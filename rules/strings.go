//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// LinesIteration detects manual line splitting patterns and suggests strings.Lines.
//
// Old pattern:
//
//	for _, line := range strings.Split(s, "\n") {
//	    process(line)
//	}
//
// New pattern (Go 1.24+):
//
//	for line := range strings.Lines(s) {
//	    process(line)
//	}
//
// Benefits:
//   - No intermediate slice allocation
//   - Handles both \n and \r\n line endings
//   - More memory efficient for large strings
//
// See: https://pkg.go.dev/strings#Lines
// See: https://pkg.go.dev/bytes#Lines
// Note: strings.Lines is not a drop-in value-equivalent of ranging over
// strings.Split: Lines keeps the line terminator (the trailing \n, and \r\n)
// on each yielded line, whereas Split strips the separator. Callers that
// compare or trim lines must account for that, so this stays Report-only.
func LinesIteration(m dsl.Matcher) {
	// Pattern: for _, line := range strings.Split(s, "\n")
	m.Match(
		`for $_, $line := range strings.Split($s, "\n") { $*body }`,
	).
		Report(`use for $line := range strings.Lines($s) instead of ranging over strings.Split($s, "\n") (Go 1.24+); not value-equivalent: Lines keeps the trailing newline on each line, Split strips it`)

	// Pattern: for _, line := range strings.Split(s, "\r\n")
	m.Match(
		`for $_, $line := range strings.Split($s, "\r\n") { $*body }`,
	).
		Report(`use for $line := range strings.Lines($s) instead of ranging over strings.Split($s, "\r\n") (Go 1.24+); not value-equivalent: Lines keeps the line terminator, Split strips it`)

	// Also detect bytes.Split for line iteration
	m.Match(
		`for $_, $line := range bytes.Split($s, []byte("\n")) { $*body }`,
	).
		Report(`use for $line := range bytes.Lines($s) instead of ranging over bytes.Split($s, []byte("\n")) (Go 1.24+); not value-equivalent: Lines keeps the trailing newline, Split strips it`)

	m.Match(
		`for $_, $line := range bytes.Split($s, []byte("\r\n")) { $*body }`,
	).
		Report(`use for $line := range bytes.Lines($s) instead of ranging over bytes.Split($s, []byte("\r\n")) (Go 1.24+); not value-equivalent: Lines keeps the line terminator, Split strips it`)

	m.Match(
		`for $_, $line := range bytes.Split($s, []byte{'\n'}) { $*body }`,
	).
		Report(`use for $line := range bytes.Lines($s) instead of ranging over bytes.Split (Go 1.24+); not value-equivalent: Lines keeps the trailing newline, Split strips it`)
}

// FieldsFuncIteration detects strings.FieldsFunc used only for iteration
// and suggests strings.FieldsFuncSeq.
//
// Old pattern:
//
//	for _, field := range strings.FieldsFunc(s, f) {
//	    process(field)
//	}
//
// New pattern (Go 1.24+):
//
//	for field := range strings.FieldsFuncSeq(s, f) {
//	    process(field)
//	}
//
// See: https://pkg.go.dev/strings#FieldsFuncSeq
// See: https://pkg.go.dev/bytes#FieldsFuncSeq
func FieldsFuncIteration(m dsl.Matcher) {
	m.Match(
		`for $_, $field := range strings.FieldsFunc($s, $f) { $*body }`,
	).
		Report("use for $field := range strings.FieldsFuncSeq($s, $f) to avoid intermediate slice allocation (Go 1.24+)")

	m.Match(
		`for $_, $field := range bytes.FieldsFunc($s, $f) { $*body }`,
	).
		Report("use for $field := range bytes.FieldsFuncSeq($s, $f) to avoid intermediate slice allocation (Go 1.24+)")
}

// CutLast detects strings.LastIndex / bytes.LastIndex followed by manual
// slicing or an index check, and suggests strings.CutLast / bytes.CutLast.
//
// Old patterns:
//
//	dir := path[:strings.LastIndex(path, "/")]
//	base := path[strings.LastIndex(path, "/")+1:]
//	if i := strings.LastIndex(s, sep); i >= 0 {
//	    before, after := s[:i], s[i+len(sep):]
//	}
//
// New pattern (Go 1.27+):
//
//	before, after, found := strings.CutLast(s, sep)
//
// CutLast returns (s, "", false) when sep is absent (bytes.CutLast returns
// (s, nil, false)). The slicing idioms behave differently in that case, so each
// Report says to check the found result: s[:LastIndex] panics on -1;
// s[LastIndex+1:] yields the whole string; s[LastIndex+len(sep):] yields the
// whole string only for a one-byte separator and drops part of it otherwise.
//
// The +1 slicing form fires only for a one-BYTE ASCII literal separator (a
// single unescaped ASCII char, or a short escape such as "\n"), because +1 skips
// exactly one byte and only then equals CutLast. The separator regex is
// restricted to the ASCII range 0x00-0x7f on purpose: a multi-byte rune literal
// like "ä" would skip into the middle of the rune, which CutLast never
// does, so it is deliberately not matched. The index-check forms fire only when
// the guarded code slices s at the index (s[:i], s[i:], s[i+n:]); an index used
// for anything else is not a CutLast candidate.
//
// See: https://pkg.go.dev/strings#CutLast
// See: https://pkg.go.dev/bytes#CutLast
func CutLast(m dsl.Matcher) {
	// Before-slice up to the separator: on a missing separator LastIndex is -1
	// and s[:-1] panics, so this is not equivalent to CutLast; check found.
	m.Match(
		`$s[:strings.LastIndex($s, $sep)]`,
	).
		Report("use before, _, found := strings.CutLast($s, $sep) instead of slicing before strings.LastIndex (Go 1.27+); when $sep is absent this slice panics (index -1) whereas CutLast returns (s, \"\", false), so check found")

	// After-slice from just past the separator.
	m.Match(
		`$s[strings.LastIndex($s, $sep)+len($sep):]`,
	).
		Report("use _, after, found := strings.CutLast($s, $sep) instead of slicing after strings.LastIndex (Go 1.27+); the not-found case differs (LastIndex is -1 when $sep is absent), so check found")

	m.Match(
		`$s[:bytes.LastIndex($s, $sep)]`,
	).
		Report("use before, _, found := bytes.CutLast($s, $sep) instead of slicing before bytes.LastIndex (Go 1.27+); when $sep is absent this slice panics (index -1) whereas bytes.CutLast returns (s, nil, false), so check found")

	m.Match(
		`$s[bytes.LastIndex($s, $sep)+len($sep):]`,
	).
		Report("use _, after, found := bytes.CutLast($s, $sep) instead of slicing after bytes.LastIndex (Go 1.27+); the not-found case differs (LastIndex is -1 when $sep is absent), so check found")

	// The +1 form skips exactly one byte, so it is a CutLast candidate only when
	// the separator is a one-character literal; with "::" it would keep a ":".
	m.Match(
		`$s[strings.LastIndex($s, $sep)+1:]`,
	).
		Where(m["sep"].Text.Matches(`^"(\\.|[\x00-\x21\x23-\x5b\x5d-\x7f])"$`)).
		Report("use before, after, found := strings.CutLast($s, $sep) instead of slicing around strings.LastIndex (Go 1.27+); when $sep is absent the slicing idiom yields all of $s but CutLast yields after == \"\", so check found")

	m.Match(
		`$s[bytes.LastIndex($s, $sep)+1:]`,
	).
		Where(m["sep"].Text.Matches(`^\[\]byte\("(\\.|[\x00-\x21\x23-\x5b\x5d-\x7f])"\)$`) || m["sep"].Text.Matches(`^\[\]byte\{'(\\.|[\x00-\x26\x28-\x5b\x5d-\x7f])'\}$`)).
		Report("use before, after, found := bytes.CutLast($s, $sep) instead of slicing around bytes.LastIndex (Go 1.27+); when $sep is absent the slicing idiom yields all of $s but CutLast yields after == nil, so check found")

	// Index check followed by manual slicing in the body.
	m.Match(
		`if $i := strings.LastIndex($s, $sep); $i >= 0 { $*body }`,
		`if $i := strings.LastIndex($s, $sep); $i != -1 { $*body }`,
		`$i := strings.LastIndex($s, $sep); if $i < 0 { $*_ }; $*body`,
		`$i := strings.LastIndex($s, $sep); if $i == -1 { $*_ }; $*body`,
	).
		Where(m["body"].Contains(`$s[:$i]`) || m["body"].Contains(`$s[$i:]`) || m["body"].Contains(`$s[$i+$_:]`)).
		Report("use before, after, found := strings.CutLast($s, $sep) instead of checking strings.LastIndex and slicing by hand (Go 1.27+)")

	m.Match(
		`if $i := bytes.LastIndex($s, $sep); $i >= 0 { $*body }`,
		`if $i := bytes.LastIndex($s, $sep); $i != -1 { $*body }`,
		`$i := bytes.LastIndex($s, $sep); if $i < 0 { $*_ }; $*body`,
		`$i := bytes.LastIndex($s, $sep); if $i == -1 { $*_ }; $*body`,
	).
		Where(m["body"].Contains(`$s[:$i]`) || m["body"].Contains(`$s[$i:]`) || m["body"].Contains(`$s[$i+$_:]`)).
		Report("use before, after, found := bytes.CutLast($s, $sep) instead of checking bytes.LastIndex and slicing by hand (Go 1.27+)")
}
