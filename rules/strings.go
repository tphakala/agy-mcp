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
