//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// MinMaxBuiltin detects the math.Min/math.Max float64-conversion form wrapped
// in an integer cast and suggests using the built-in min/max functions.
//
// Old pattern (only the math.Min/Max-with-conversion form is detected; the
// if/else form is not matched):
//
//	result := int(math.Min(float64(a), float64(b)))
//
// New pattern (Go 1.21+):
//
//	result := min(a, b)
//	result := max(a, b)
//
// Benefits:
//   - Cleaner, more readable code
//   - Works with any ordered type
//   - No type conversion needed
//
// See: https://pkg.go.dev/builtin#min
// See: https://pkg.go.dev/builtin#max
func MinMaxBuiltin(m dsl.Matcher) {
	// Report-only, no autofix. The Suggest would drop the outer integer
	// conversion (int/int64/int32) that makes the expression type-check.
	// min/max return the operand type, so for operands narrower or wider than
	// the conversion target the rewrite does not compile, and removing the only
	// math.Min/Max call orphans the "math" import (--fix does not prune imports).
	// The rewrite also needs a type constraint on the operands to be safe.

	// math.Min with float64 conversion for integers
	m.Match(
		`int(math.Min(float64($a), float64($b)))`,
	).
		Report("use min($a, $b) instead of int(math.Min(float64(...))) (Go 1.21+)")

	m.Match(
		`int64(math.Min(float64($a), float64($b)))`,
	).
		Report("use min($a, $b) instead of int64(math.Min(float64(...))) (Go 1.21+)")

	m.Match(
		`int32(math.Min(float64($a), float64($b)))`,
	).
		Report("use min($a, $b) instead of int32(math.Min(float64(...))) (Go 1.21+)")

	// math.Max with float64 conversion for integers
	m.Match(
		`int(math.Max(float64($a), float64($b)))`,
	).
		Report("use max($a, $b) instead of int(math.Max(float64(...))) (Go 1.21+)")

	m.Match(
		`int64(math.Max(float64($a), float64($b)))`,
	).
		Report("use max($a, $b) instead of int64(math.Max(float64(...))) (Go 1.21+)")

	m.Match(
		`int32(math.Max(float64($a), float64($b)))`,
	).
		Report("use max($a, $b) instead of int32(math.Max(float64(...))) (Go 1.21+)")
}

// ClearBuiltin detects loop-based map/slice clearing patterns and suggests
// using the built-in clear() function.
//
// Old pattern (only map clearing is detected; the slice-zeroing loop is not
// matched):
//
//	for k := range m {
//	    delete(m, k)
//	}
//
// New pattern (Go 1.21+):
//
//	clear(m)  // Deletes all map entries
//
// Benefits:
//   - Cleaner, more readable code
//   - More efficient (optimized implementation)
//   - Works with maps and slices
//
// See: https://pkg.go.dev/builtin#clear
func ClearBuiltin(m dsl.Matcher) {
	// Map clearing pattern: for k := range m { delete(m, k) }
	m.Match(
		`for $k := range $m { delete($m, $k) }`,
	).
		Report("use clear($m) instead of loop-based map clearing (Go 1.21+)").
		Suggest("clear($m)")

	// Map clearing with underscore value: for k, _ := range m { delete(m, k) }
	m.Match(
		`for $k, _ := range $m { delete($m, $k) }`,
	).
		Report("use clear($m) instead of loop-based map clearing (Go 1.21+)").
		Suggest("clear($m)")
}

// RangeOverInteger detects traditional for loops that iterate from 0 to n
// and suggests using the Go 1.22+ range-over-integer syntax.
//
// Old pattern:
//
//	for i := 0; i < n; i++ {
//	    process(i)
//	}
//
// New pattern (Go 1.22+):
//
//	for i := range n {
//	    process(i)
//	}
//
// Benefits:
//   - More concise and readable
//   - Intent is clearer (iterate n times)
//   - Less error-prone (no off-by-one mistakes)
//
// Note: Only matches loops starting from 0 with < comparison and i++.
// Loops with different starting values, comparisons, or increments
// are intentionally not flagged.
//
// See: https://go.dev/doc/go1.22#language
func RangeOverInteger(m dsl.Matcher) {
	// Pattern: for i := 0; i < n; i++
	// Report-only, no autofix. Rewriting to `for i := range n` evaluates n once
	// at loop entry, so any loop whose body mutates n (a worklist that appends to
	// the slice it ranges, for example) silently changes behavior while still
	// compiling. The range form also drops i when the body never reads it, which
	// would leave an "unused variable" autofix.
	//
	// Exclusions:
	//   - benchmark loops (b.N) should use b.Loop() instead
	//   - reflect Num* counters
	//   - single-statement spread-append bodies, which are SliceRepeat's pattern;
	//     without this, RangeOverInteger (loaded first, first match wins) shadows
	//     the more specific "use slices.Repeat" advice.
	m.Match(
		`for $i := 0; $i < $n; $i++ { $*body }`,
	).
		Where(
			!m["n"].Text.Matches(`.*\.N$`) &&
				!m["n"].Text.Matches(`\.(NumField|NumMethod|NumIn|NumOut)\(\)$`) &&
				!m["body"].Text.Matches(`(?s)^\s*\S+\s*=\s*append\(.*?\.\.\.\)\s*$`),
		).
		Report("use for $i := range $n instead of for $i := 0; $i < $n; $i++ (Go 1.22+)")
}

// AppendWithoutValues detects append calls with no values which have no effect.
//
// Broken pattern:
//
//	slice = append(slice)  // No effect
//
// See: https://pkg.go.dev/builtin#append
// Note: Go 1.22 vet tool also warns about this pattern.
func AppendWithoutValues(m dsl.Matcher) {
	m.Match(
		`append($s)`,
	).
		Report("append with single argument has no effect; did you forget the values to append?")
}

// NewWithExpression detects the slice-literal hack for getting a pointer to a value
// and suggests using Go 1.26's enhanced new() built-in.
//
// Old pattern (slice hack):
//
//	field := &[]string{"hello"}[0]
//	field := &[]int{42}[0]
//	field := &[]time.Duration{5 * time.Second}[0]
//
// New pattern (Go 1.26+):
//
//	field := new("hello")
//	field := new(42)
//	field := new(5 * time.Second)
//
// Benefits:
//   - Eliminates the obscure slice-literal-index hack
//   - Clearer intent: "pointer to this value"
//   - No intermediate slice allocation
//   - Works with any expression, including function calls
//
// See: https://go.dev/doc/go1.26#language
func NewWithExpression(m dsl.Matcher) {
	// Pattern: &[]T{v}[0] - the well-known slice hack for pointer-to-value
	// Note: Report-only, no autofix. The replacement new($val) must preserve the type of $typ,
	// which requires new($typ($val)) for type conversions. Since we can't reliably determine
	// when a type conversion is needed, we report without suggesting an autofix.
	m.Match(
		`&[]$typ{$val}[0]`,
	).
		Report("consider using new($typ($val)) instead of &[]$typ{$val}[0] (Go 1.26+); verify type compatibility")
}
