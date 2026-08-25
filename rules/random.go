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

// RandMethodN detects a typed wrapper around a *rand.Rand bounded-integer method
// and suggests the generic Rand.N method, which Go 1.27's generic methods made
// possible.
//
// Old pattern (math/rand/v2, custom source):
//
//	r := rand.New(rand.NewPCG(1, 2))
//	jitter := time.Duration(r.Int64N(int64(maxJitter)))
//	idx := myIndex(r.IntN(int(n)))
//
// New pattern (Go 1.27+):
//
//	jitter := r.N(maxJitter)
//	idx := r.N(n)
//
// Rand.N mirrors the top-level rand.N function: the type parameter is inferred
// from the argument, so the two conversions disappear. The rule fires only when
// the outer call is a conversion to the argument's own type, so r.N($n) yields
// exactly the type the old expression produced. Ordinary function calls
// wrapping IntN, and conversions to some other type, are left alone. There is
// no auto-fix on purpose: int64(r.Int32N(int32(n))) with an int64 n matches,
// and r.N(n) would use the untruncated bound. It does not fire on the global functions (rand.Int64N)
// because rand.N has covered those since Go 1.22.
//
// m.Import is required: ruleguard resolves the package name in a type pattern
// through its standard library table, where "rand" means math/rand, so without
// it "*rand.Rand" never matches the v2 type. MEASURED against golangci-lint
// 2.13.1 (ruleguard 0.4.5) on 2026-08-25: with the import the rule fires on
// every planted site; a full import path in the type string, as in
// "*math/rand/v2.Rand", is not a fix, it silently disables every rule in rules/.
//
// See: https://pkg.go.dev/math/rand/v2#Rand.N
func RandMethodN(m dsl.Matcher) {
	m.Import("math/rand/v2")

	m.Match(
		`$T($r.Int64N(int64($n)))`,
		`$T($r.Int32N(int32($n)))`,
		`$T($r.IntN(int($n)))`,
		`$T($r.Uint64N(uint64($n)))`,
		`$T($r.Uint32N(uint32($n)))`,
		`$T($r.UintN(uint($n)))`,
	).
		Where(m["r"].Type.Is("*rand.Rand") && m["T"].Type.IdenticalTo(m["n"])).
		Report("use $r.N($n) instead of converting through a fixed-width bounded method; Rand.N infers the integer type from $n (Go 1.27+); not a drop-in if the inner conversion truncated $n")
}
