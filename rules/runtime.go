//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// PreferAddCleanup detects runtime.SetFinalizer and suggests runtime.AddCleanup.
//
// runtime.SetFinalizer is NOT deprecated (no Deprecated: tag as of Go 1.27,
// verified: staticcheck SA1019 does not flag it, unlike tls.Config.Rand), so
// this is an advisory preference, not a deprecation warning. AddCleanup (Go 1.24)
// is the recommended API for new code.
//
// The old pattern:
//
//	runtime.SetFinalizer(obj, func(o *Type) { cleanup(o) })
//
// New pattern (Go 1.24+):
//
//	runtime.AddCleanup(obj, func(arg ArgType) { cleanup(arg) }, arg)
//
// Benefits of AddCleanup:
//   - Multiple cleanups per object
//   - Can attach to interior pointers
//   - No cycle leaks (SetFinalizer can leak cycles)
//   - Doesn't delay object freeing
//   - Cleaner API with explicit cleanup argument
//
// See: https://pkg.go.dev/runtime#AddCleanup
func PreferAddCleanup(m dsl.Matcher) {
	m.Match(
		`runtime.SetFinalizer($obj, $fn)`,
	).
		Report("consider using runtime.AddCleanup instead of runtime.SetFinalizer (Go 1.24+): AddCleanup allows multiple cleanups, avoids cycle leaks, and doesn't delay object freeing")
}
