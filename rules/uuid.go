//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// StdlibUUID detects the github.com/google/uuid API and suggests the standard
// library uuid package added in Go 1.27.
//
// Old pattern:
//
//	import "github.com/google/uuid"
//	id := uuid.New()
//	s := uuid.NewString()
//	parsed, err := uuid.Parse(s)
//
// New pattern (Go 1.27+):
//
//	import "uuid"
//	id := uuid.New()
//	s := uuid.New().String()
//	parsed, err := uuid.Parse(s)
//
// The standard package implements RFC 9562, draws random bytes from crypto/rand,
// and covers the common surface: New (v4), NewV4, NewV7, Parse, MustParse, Nil,
// Max, plus String and the encoding.TextMarshaler pair on UUID. Dropping the
// dependency is only worth it when nothing else in the module needs the extra
// google/uuid API (v1, v5, SQL scanning, ClockSequence), so this is advisory.
//
// See: https://pkg.go.dev/uuid
func StdlibUUID(m dsl.Matcher) {
	m.Match(
		`uuid.New()`,
		`uuid.NewString()`,
		`uuid.NewRandom()`,
		`uuid.NewV7()`,
		`uuid.Parse($s)`,
		`uuid.MustParse($s)`,
		`uuid.Nil`,
		`uuid.Max`,
	).
		Where(m.File().Imports("github.com/google/uuid")).
		Report("the standard library uuid package (Go 1.27+) covers New/NewV4/NewV7/Parse/MustParse/Nil/Max; consider dropping github.com/google/uuid (its NewRandom/NewV7 lose the error return, and Nil/Max become functions)")
}
