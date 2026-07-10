//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// WaitGroupGo detects the old sync.WaitGroup pattern, passed by reference to a
// closure parameter, and suggests using Go 1.25's wg.Go().
//
// The direct-closure form (wg.Add(1); go func() { defer wg.Done(); ... }())
// is not matched here: the now-enabled modernize linter's waitgroup analyzer
// already catches it, with autofix, verified to emit the identical rewrite
// this rule's own former Suggest() produced. This rule keeps only the
// param-passed form modernize does not cover:
//
//	wg.Add(1)
//	go func(w *sync.WaitGroup) {
//	    defer w.Done()
//	    doSomething()
//	}(wg)
//
// Can be simplified to:
//
//	wg.Go(func() {
//	    doSomething()
//	})
//
// Benefits:
//   - Cleaner, less error-prone (no Add/Done mismatch)
//   - Single function call
//
// Note: wg.Go still calls Done on panic (it is deferred), but it does not
// recover the panic; a panicking task crashes the program just as before.
//
// See: https://pkg.go.dev/sync#WaitGroup.Go
func WaitGroupGo(m dsl.Matcher) {
	m.Match(
		`$wg.Add(1); go func($param $typ) { defer $param.Done(); $*body }($wg)`,
		`$wg.Add(1); go func($param $typ) { defer $param.Done(); $*body }(&$wg)`,
	).
		Where(m["wg"].Type.Is("*sync.WaitGroup") || m["wg"].Type.Is("sync.WaitGroup")).
		Report("use $wg.Go(func() { $body }) instead of manual Add/Done pattern (Go 1.25+)")
}
