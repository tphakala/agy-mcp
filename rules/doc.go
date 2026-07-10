//go:build ruleguard

// Package gorules defines custom ruleguard matchers for Go modernization.
//
// The matchers are loaded by golangci-lint's gocritic ruleguard checker (see
// settings.gocritic.settings.ruleguard.rules in .golangci.yaml). Every file
// carries the //go:build ruleguard tag so the normal Go toolchain ignores
// them; `go build -tags ruleguard ./rules/` compiles them as a canary.
//
// # Overlap with govet and staticcheck
//
// A few matchers deliberately duplicate a stock analyzer instead of ceding to
// it, because .golangci.yaml sets issues.uniq-by-line: true: when two linters
// flag the same line, only one message survives and the winner is arbitrary
// run-order, not a quality choice. Two outcomes follow from that:
//
//   - Exact duplicates with no informational gain over the stock analyzer are
//     deleted rather than kept as dead weight (AppendWithoutValues and
//     DeferredTimeSince were removed: govet's default appends/defers
//     analyzers already flag the identical patterns with an equivalent
//     message; verified with `go vet` and a minimal govet-only golangci-lint
//     config). DeferredTimeNow stays: govet's defers analyzer does not flag a
//     bare deferred time.Now(), only defer-wrapped time.Since().
//   - The crypto.go and reflect.go Deprecated* matchers keep duplicating
//     staticcheck's SA1019 anyway: their messages name the concrete attack
//     class (Bleichenbacher, active-attack vulnerability in unauthenticated
//     cipher modes, etc.) and the specific replacement API, which is more
//     actionable than SA1019's generic "X is deprecated" text. Sometimes
//     staticcheck's message wins the uniq-by-line lottery instead and the
//     richer text doesn't show, but the line is still flagged either way, so
//     this only costs message quality, never a missed violation. That trade
//     is accepted deliberately.
//
// JoinHostPort and TestingContext were removed in favor of enabling the
// nosprintfhostport and usetesting stock linters (see .golangci.yaml), which
// cover the same ground with proper type/control-flow analysis instead of
// ruleguard's syntactic matching. This also fixes TestingContext's known
// false positive in TestMain/benchmarks (ruleguard matches by file name and
// cannot see the enclosing function signature; usetesting can).
package gorules
