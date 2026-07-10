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
//   - Some Deprecated* matchers (crypto.go, reflect.go, and net.go's
//     DeprecatedReverseProxyDirector) keep duplicating staticcheck's SA1019
//     anyway: their messages name the concrete attack class (Bleichenbacher,
//     active-attack vulnerability in unauthenticated cipher modes, hop-by-hop
//     header abuse, etc.) and the specific replacement API, which is more
//     actionable than SA1019's generic "X is deprecated" text. Sometimes
//     staticcheck's message wins the uniq-by-line lottery instead and the
//     richer text doesn't show, but the line is still flagged either way, so
//     this only costs message quality, never a missed violation. That trade
//     is accepted deliberately. This is not a full audit of every
//     Deprecated*-flavored matcher in the package against SA1019 or against
//     the now-enabled modernize linter; see #77 for the remaining candidates.
//
// TestingContext was removed in favor of enabling the stock usetesting
// linter with its context-background and context-todo settings explicitly
// turned on (see .golangci.yaml linters.settings.usetesting; both default to
// false upstream, so enabling the linter alone does NOT reproduce
// TestingContext's coverage, verified empirically). With those settings on,
// usetesting also fixes TestingContext's known false positive in
// TestMain/benchmarks, since it has real control-flow visibility into the
// enclosing function signature where ruleguard's file-name-only matching did
// not. Enabling usetesting's os-mkdir-temp check as a side effect newly
// overlaps testing.go's TestingArtifactDir matcher; that pair is kept
// deliberately for the same richer-message reason as the SA1019 duplicates
// above.
//
// JoinHostPort was kept, not removed: the stock nosprintfhostport linter
// only matches a scheme-prefixed URL ("scheme://%s:..."), never the bare
// "%s:%d"/"%v:%d" pattern JoinHostPort targets (the common case for building
// a raw net.Dial/net.Listen/http.Server.Addr address), verified empirically.
// nosprintfhostport is enabled anyway as complementary coverage for the
// URL-embedded case, which JoinHostPort deliberately does not flag.
package gorules
