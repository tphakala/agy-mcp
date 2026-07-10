//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// ReflectTypeAssert detects v.Interface().(T) pattern and suggests reflect.TypeAssert.
//
// The old pattern (allocates):
//
//	val := v.Interface().(string)
//	val, ok := v.Interface().(string)
//
// New pattern (Go 1.25+, no allocation):
//
//	val := reflect.TypeAssert[string](v)
//	val, ok := reflect.TypeAssert[string](v)
//
// reflect.TypeAssert converts Value directly to typed value without intermediate
// allocation via Interface(). This is more efficient for hot paths.
//
// See: https://pkg.go.dev/reflect#TypeAssert
func ReflectTypeAssert(m dsl.Matcher) {
	// Pattern: Type assertion on Interface() result
	m.Match(
		`$v.Interface().($typ)`,
	).
		Where(m["v"].Type.Is("reflect.Value")).
		Report("use reflect.TypeAssert[$typ]($v) instead of $v.Interface().($typ) to avoid allocation (Go 1.25+)")
}

// DeprecatedReflectPtrTo detects deprecated reflect.PtrTo and suggests
// reflect.PointerTo. Named with the Deprecated* prefix for consistency with
// this package's other stdlib-deprecation matchers (crypto.go, net.go).
//
// Deprecated pattern:
//
//	ptrType := reflect.PtrTo(t)
//
// New pattern (Go 1.22+):
//
//	ptrType := reflect.PointerTo(t)
//
// reflect.PtrTo was deprecated in Go 1.22 in favor of the clearer name PointerTo.
//
// See: https://pkg.go.dev/reflect#PointerTo
func DeprecatedReflectPtrTo(m dsl.Matcher) {
	m.Match(
		`reflect.PtrTo($t)`,
	).
		Report("reflect.PtrTo is deprecated in Go 1.22; use reflect.PointerTo($t) instead").
		Suggest("reflect.PointerTo($t)")
}
