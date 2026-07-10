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

// ReflectFieldsIterator detects manual index-based iteration over struct fields
// and suggests using the iterator methods added in Go 1.26.
//
// Kept deliberately duplicating modernize's stditerators analyzer for the
// single-target case (see rules/doc.go): verified stditerators silently
// declines to fire at all on the common "parallel Type+Value indexed access"
// pattern below (sf := t.Field(i); vf := val.Field(i), same i), rather than
// risking an unsafe fused rewrite. This rule still fires unconditionally on
// any Type/Value.NumField loop, parallel-access or not, so it is the only
// remaining signal for that pattern; the tradeoff is losing the
// uniq-by-line lottery to stditerators' autofix on the simple case.
//
// Old pattern:
//
//	for i := 0; i < t.NumField(); i++ {
//	    f := t.Field(i)
//	    // use f
//	}
//
// New patterns (Go 1.26+):
//
//	for f := range t.Fields() {       // reflect.Type
//	    // use f
//	}
//	for sf, v := range val.Fields() { // reflect.Value
//	    // use sf (StructField) and v (Value)
//	}
//
// Benefits:
//   - Cleaner, more idiomatic Go iteration
//   - No off-by-one risk
//   - Consistent with Go 1.23+ iterator patterns
//   - Reduces boilerplate
//
// See: https://pkg.go.dev/reflect#Type.Fields
// See: https://pkg.go.dev/reflect#Value.Fields
func ReflectFieldsIterator(m dsl.Matcher) {
	// Type.NumField loop
	m.Match(
		`for $i := 0; $i < $t.NumField(); $i++ { $*_ }`,
	).
		Where(m["t"].Type.Is("reflect.Type")).
		Report("use range $t.Fields() instead of index-based field iteration (Go 1.26+); if the loop index is also used for reflect.Value field access, range over the Value instead")

	// Value.NumField loop
	m.Match(
		`for $i := 0; $i < $v.NumField(); $i++ { $*_ }`,
	).
		Where(m["v"].Type.Is("reflect.Value")).
		Report("use range $v.Fields() instead of index-based field iteration (Go 1.26+)")

	// range over NumField integer (Go 1.22+ style)
	m.Match(
		`for $i := range $t.NumField() { $*_ }`,
	).
		Where(m["t"].Type.Is("reflect.Type")).
		Report("use range $t.Fields() instead of range $t.NumField() (Go 1.26+); if the loop index is also used for reflect.Value field access, range over the Value instead")

	m.Match(
		`for $i := range $v.NumField() { $*_ }`,
	).
		Where(m["v"].Type.Is("reflect.Value")).
		Report("use range $v.Fields() instead of range $v.NumField() (Go 1.26+)")
}

// ReflectMethodsIterator detects manual index-based iteration over type methods
// and suggests using the iterator methods added in Go 1.26.
//
// Kept deliberately for the identical reason as ReflectFieldsIterator above:
// modernize's stditerators declines to fire on the parallel Type+Value
// indexed access pattern (m := t.Method(i); v := val.Method(i), same i).
//
// Old pattern:
//
//	for i := 0; i < t.NumMethod(); i++ {
//	    m := t.Method(i)
//	    // use m
//	}
//
// New patterns (Go 1.26+):
//
//	for m := range t.Methods() {       // reflect.Type
//	    // use m
//	}
//	for m, v := range val.Methods() {  // reflect.Value
//	    // use m (Method) and v (Value)
//	}
//
// Benefits:
//   - Cleaner, more idiomatic Go iteration
//   - No off-by-one risk
//   - Consistent with Go 1.23+ iterator patterns
//
// See: https://pkg.go.dev/reflect#Type.Methods
// See: https://pkg.go.dev/reflect#Value.Methods
func ReflectMethodsIterator(m dsl.Matcher) {
	// Type.NumMethod loop
	m.Match(
		`for $i := 0; $i < $t.NumMethod(); $i++ { $*_ }`,
	).
		Where(m["t"].Type.Is("reflect.Type")).
		Report("use range $t.Methods() instead of index-based method iteration (Go 1.26+)")

	// Value.NumMethod loop
	m.Match(
		`for $i := 0; $i < $v.NumMethod(); $i++ { $*_ }`,
	).
		Where(m["v"].Type.Is("reflect.Value")).
		Report("use range $v.Methods() instead of index-based method iteration (Go 1.26+)")

	// range over NumMethod integer
	m.Match(
		`for $i := range $t.NumMethod() { $*_ }`,
	).
		Where(m["t"].Type.Is("reflect.Type")).
		Report("use range $t.Methods() instead of range $t.NumMethod() (Go 1.26+)")

	m.Match(
		`for $i := range $v.NumMethod() { $*_ }`,
	).
		Where(m["v"].Type.Is("reflect.Value")).
		Report("use range $v.Methods() instead of range $v.NumMethod() (Go 1.26+)")
}
