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
//   - Some Deprecated* matchers (crypto.go, reflect.go's
//     DeprecatedCipherModes/DeprecatedElliptic/DeprecatedRSAMultiPrime/
//     DeprecatedPKCS1v15, and net.go's DeprecatedReverseProxyDirector) keep
//     duplicating staticcheck's SA1019 anyway: their messages name the
//     concrete attack class (Bleichenbacher, active-attack vulnerability in
//     unauthenticated cipher modes, hop-by-hop header abuse, etc.) and the
//     specific replacement API, which is more actionable than SA1019's
//     generic "X is deprecated" text. Sometimes staticcheck's message wins
//     the uniq-by-line lottery instead and the richer text doesn't show, but
//     the line is still flagged either way, so this only costs message
//     quality, never a missed violation. That trade is accepted
//     deliberately.
//   - random.go's rand.Seed keeps duplicating SA1019 for the same reason,
//     plus a capability SA1019 cannot replicate: the message substitutes the
//     actual matched $seed into the suggested replacement call, so the fix
//     shown is already correct for that call site, not generic prose.
//     rand.Read's submatch was removed instead: verified SA1019's message
//     names both replacements (crypto/rand.Read and, for a deterministic
//     non-crypto source, math/rand/v2.ChaCha8.Read), while this rule's
//     message named only the first, so SA1019 is strictly more complete here.
//   - runtime.go's GorootDeprecated was removed: verified SA1019's message
//     for runtime.GOROOT is more detailed than this rule's, with no
//     offsetting gain (no $capture substitution, unlike rand.Seed above).
//   - builtins.go's RangeOverInteger was removed: verified the now-enabled
//     modernize linter's rangeint analyzer has equal-or-better safety
//     awareness (it correctly skips a loop bound mutated in the body, skips
//     b.N benchmark loops, and defers to its own better stditerators
//     suggestion for reflect Type.NumField loops -- all cases this rule also
//     excluded) plus autofix, which this rule deliberately declined to offer
//     for the same safety reasons. Dropping it is not just deduplication;
//     modernize is a strict upgrade here. This leaves a latent, pre-existing,
//     out-of-scope collision unfixed: SliceRepeat's spread-append pattern
//     (slices.go) is also matched by modernize's rangeint with its generic
//     message, so uniq-by-line arbitrarily picks between the two on that
//     line; not addressed here since it is not new and not caused by
//     removing RangeOverInteger.
//   - sync.go's WaitGroupGo kept only its param-passed-closure pattern and
//     dropped the direct-closure one: verified modernize's waitgroup
//     analyzer catches the direct form with autofix producing an identical
//     rewrite to this rule's own former Suggest(), but does not catch the
//     param-passed form at all, so that half is unique coverage.
//   - testing.go's BenchmarkLoop and errors.go's ErrorsAsType were checked
//     against modernize and kept unchanged: golangci-lint v2.12.2's
//     modernize does not implement a b.Loop() or errors.AsType analyzer at
//     all (confirmed against its configurable analyzer list), so these
//     provide genuine, currently-unique coverage. Revisit on a future
//     golangci-lint bump.
//   - reflect.go's ReflectPtrTo was renamed to DeprecatedReflectPtrTo, and
//     net.go's DeprecatedReverseProxyDirector's assignment-form match was
//     extended to accept both *httputil.ReverseProxy and the value type
//     (matching TimeDateTimeConstants's identical both-forms pattern in
//     time.go), for consistency and completeness rather than an overlap fix.
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
