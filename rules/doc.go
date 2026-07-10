//go:build ruleguard

// Package gorules defines custom ruleguard matchers for Go modernization.
//
// The matchers are loaded by golangci-lint's gocritic ruleguard checker (see
// settings.gocritic.settings.ruleguard.rules in .golangci.yaml). Every file
// carries the //go:build ruleguard tag so the normal Go toolchain ignores
// them; `go build -tags ruleguard ./rules/` compiles them as a canary.
//
// # Overlap with govet, staticcheck, and modernize
//
// A few matchers deliberately duplicate a stock golangci-lint analyzer
// (govet, staticcheck's SA1019, or the modernize linter) instead of ceding
// to it, because .golangci.yaml sets issues.uniq-by-line: true: when two
// linters flag the same line, only one message survives and the winner is
// arbitrary run-order, not a quality choice. The policy: verify with a
// throwaway golangci-lint repro against the pinned toolchain version before
// deciding, drop a matcher only when the stock analyzer is confirmed an
// exact-or-better replacement (same trigger, equal-or-better safety
// exclusions, ideally autofix), and keep a matcher that adds genuine
// informational value (a richer message, a $capture-substituted concrete
// fix, or detection scope the stock analyzer lacks) even at the cost of
// occasionally losing the uniq-by-line lottery.
//
// Dropped as exact-or-inferior duplicates (verified against govet/
// staticcheck/modernize on the pinned golangci-lint version; each of these
// used to be a distinct matcher function, now gone):
//
//   - AppendWithoutValues, DeferredTimeSince: govet's default appends/defers
//     analyzers already flag the identical patterns with an equivalent
//     message.
//   - runtime.go's GorootDeprecated: SA1019's message for runtime.GOROOT is
//     more detailed, with no offsetting gain.
//   - random.go's rand.Read submatch (RandV2Migration kept its other
//     clauses): SA1019 names both replacements (crypto/rand.Read and, for a
//     deterministic non-crypto source, math/rand/v2.ChaCha8.Read); this
//     rule's message named only the first.
//   - reflect.go's DeprecatedReflectHeaders (SliceHeader/StringHeader): same
//     reason, SA1019 names both replacements per symbol where this rule
//     named only one.
//   - builtins.go's RangeOverInteger, reflect.go's ReflectTypeOf,
//     ReflectFieldsIterator, ReflectMethodsIterator, ReflectInsOutsIterator:
//     modernize's rangeint/reflecttypefor/stditerators analyzers cover the
//     identical patterns (rangeint additionally respects the same safety
//     exclusions RangeOverInteger did: a mutated loop bound, b.N benchmark
//     loops, reflect Num* counters; reflecttypefor also matches a
//     bare-variable form ReflectTypeOf did not) with autofix, which these
//     rules deliberately declined to offer for safety/import-pruning
//     reasons. Not just deduplication -- modernize is a strict upgrade.
//   - strings.go's SplitIteration and FieldsIteration (both the strings.*
//     and bytes.* forms in each): modernize's stringsseq analyzer matches
//     the identical patterns with autofix, including the newline-separator
//     case SplitIteration deliberately excluded (see the LinesIteration
//     note below).
//   - sync.go's WaitGroupGo direct-closure pattern (the param-passed-closure
//     pattern stayed): modernize's waitgroup analyzer's autofix produces an
//     identical rewrite; it does not match the param-passed form at all.
//   - slices.go's BackwardIteration "i >= 0" pattern (the "i > -1" pattern
//     stayed): modernize's slicesbackward analyzer matches it, already
//     correctly slice-type-guarded (no false positive on strings) with
//     autofix; it does not recognize the "i > -1" spelling at all.
//
// Kept deliberately despite duplicating a stock analyzer, because the
// message adds genuine value beyond what uniq-by-line's arbitrary winner
// would otherwise show:
//
//   - time.go's DeferredTimeNow: govet's defers analyzer does not flag a
//     bare deferred time.Now(), only defer-wrapped time.Since() -- this is
//     unique coverage, not a duplicate.
//   - crypto.go's DeprecatedCipherModes/DeprecatedElliptic/
//     DeprecatedRSAMultiPrime/DeprecatedPKCS1v15, net.go's
//     DeprecatedReverseProxyDirector: messages name the concrete attack
//     class (Bleichenbacher, active-attack vulnerability in unauthenticated
//     cipher modes, hop-by-hop header abuse, etc.), more actionable than
//     SA1019's generic "X is deprecated" text.
//   - random.go's rand.Seed: duplicates SA1019, plus a capability SA1019
//     cannot replicate -- the message substitutes the actual matched $seed
//     into the suggested replacement call, so the fix shown is already
//     correct for that call site, not generic prose.
//   - strings.go's LinesIteration: no modernize Lines()-suggesting analyzer
//     exists, but modernize's stringsseq analyzer DOES fire on the same
//     for-range-Split("\n") pattern LinesIteration matches (suggesting
//     SplitSeq, which is value-equivalent but not the line-oriented idiom),
//     so this is a latent, accepted uniq-by-line collision, not a clean
//     non-overlap. LinesIteration's message states the value-equivalence
//     caveat (Lines keeps the trailing newline, Split/SplitSeq strip it)
//     that stringsseq's does not.
//   - slices.go's SliceRepeat: modernize's rangeint analyzer also matches
//     its spread-append pattern with a generic message; same latent,
//     accepted uniq-by-line collision, pre-existing and not addressed here.
//
// Checked against modernize and confirmed as genuinely unique, currently no
// overlap at all (revisit on a future golangci-lint bump):
//
//   - testing.go's BenchmarkLoop, errors.go's ErrorsAsType: golangci-lint
//     v2.12.2's modernize does not implement a b.Loop() or errors.AsType
//     analyzer (confirmed against its configurable analyzer list).
//   - builtins.go's MinMaxBuiltin, ClearBuiltin: modernize's minmax
//     analyzer only handles the if/else form, not MinMaxBuiltin's
//     int(math.Min(float64(...))) conversion form; no clear()-suggesting
//     analyzer exists in this golangci-lint version at all.
//   - reflect.go's ReflectTypeAssert: no overlap found.
//
// Cosmetic, not an overlap fix: reflect.go's ReflectPtrTo was renamed to
// DeprecatedReflectPtrTo for naming consistency with this package's other
// stdlib-deprecation matchers; net.go's DeprecatedReverseProxyDirector's
// assignment-form match was extended to accept both *httputil.ReverseProxy
// and the value type (matching TimeDateTimeConstants's identical
// both-forms pattern in time.go).
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
