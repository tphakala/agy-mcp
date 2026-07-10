//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// RandV2Migration detects math/rand usage and suggests migrating to math/rand/v2.
//
// Go 1.20 deprecated (global rand is auto-seeded since 1.20):
//   - rand.Seed() - use rand.New(rand.NewSource(seed)) for reproducibility
//
// rand.Read() is also Go 1.20-deprecated but is not matched here: staticcheck's
// SA1019 covers it with a message naming both replacements (crypto/rand.Read
// for security, math/rand/v2.ChaCha8.Read for a deterministic non-crypto
// source), which is more complete than this rule's single-alternative text.
//
// Go 1.22 introduced math/rand/v2 with improved APIs:
//
// Method renames:
//   - rand.Intn(n) → rand.IntN(n)
//   - rand.Int31() → rand.Int32()
//   - rand.Int31n(n) → rand.Int32N(n)
//   - rand.Int63() → rand.Int64()
//   - rand.Int63n(n) → rand.Int64N(n)
//
// New features:
//   - rand.N[T](max) - generic version for any integer type
//   - Better random number generation algorithms
//   - No need to seed (auto-seeded)
//
// Note: This rule flags math/rand usage to encourage migration.
// The v2 API is cleaner and more consistent.
//
// See: https://pkg.go.dev/math/rand/v2
func RandV2Migration(m dsl.Matcher) {
	// rand.Intn → rand.IntN
	m.Match(
		`rand.Intn($n)`,
	).
		Report("consider using math/rand/v2: rand.IntN($n) instead of rand.Intn (Go 1.22+)")

	// rand.Int31 → rand.Int32
	m.Match(
		`rand.Int31()`,
	).
		Report("consider using math/rand/v2: rand.Int32() instead of rand.Int31 (Go 1.22+)")

	// rand.Int31n → rand.Int32N
	m.Match(
		`rand.Int31n($n)`,
	).
		Report("consider using math/rand/v2: rand.Int32N($n) instead of rand.Int31n (Go 1.22+)")

	// rand.Int63 → rand.Int64
	m.Match(
		`rand.Int63()`,
	).
		Report("consider using math/rand/v2: rand.Int64() instead of rand.Int63 (Go 1.22+)")

	// rand.Int63n → rand.Int64N
	m.Match(
		`rand.Int63n($n)`,
	).
		Report("consider using math/rand/v2: rand.Int64N($n) instead of rand.Int63n (Go 1.22+)")

	// rand.Seed is deprecated (Go 1.20+, auto-seeded). Kept deliberately despite
	// duplicating staticcheck's SA1019 on this symbol: the message substitutes
	// the actual matched $seed into the replacement call, giving a concrete,
	// copy-pasteable fix SA1019's generic prose cannot produce.
	m.Match(
		`rand.Seed($seed)`,
	).
		Report("rand.Seed is deprecated (Go 1.20+); global rand is auto-seeded; use rand.New(rand.NewSource($seed)) for reproducibility")
}
