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
//     ReflectInsOutsIterator: modernize's rangeint/reflecttypefor/
//     stditerators analyzers cover the identical patterns (rangeint
//     additionally respects the same safety exclusions RangeOverInteger
//     did: a mutated loop bound, b.N benchmark loops, reflect Num*
//     counters; reflecttypefor also matches a bare-variable form
//     ReflectTypeOf did not) with autofix, which these rules deliberately
//     declined to offer for safety/import-pruning reasons. Not just
//     deduplication -- modernize is a strict upgrade. ReflectInsOutsIterator
//     has no reflect.Value-side equivalent (NumIn/NumOut are Type-only, for
//     function signatures), so unlike ReflectFieldsIterator/
//     ReflectMethodsIterator below there is no parallel-access case to
//     preserve; the removal is clean.
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
//   - reflect.go's ReflectFieldsIterator, ReflectMethodsIterator: verified
//     modernize's stditerators silently declines to fire at all (not even a
//     worse message -- zero signal) on the common "parallel Type+Value
//     indexed access" pattern (sf := t.Field(i); vf := val.Field(i), same
//     i; likewise for Method(i)), almost certainly because it cannot safely
//     fuse two independent NumField/NumMethod-bound accesses into one
//     Fields()/Methods() iterator without an index correspondence
//     guarantee. These two rules still fire unconditionally on every
//     Type/Value NumField or NumMethod loop, so they remain the only signal
//     for the parallel-access case, at the cost of losing the uniq-by-line
//     lottery to stditerators' autofix on the simple single-target case.
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
// both-forms pattern in time.go). The short package-name form (not the
// fully-qualified "net/http/httputil.ReverseProxy") is deliberate and
// matches every other Type.Is call in this package (time.Time, sync.
// WaitGroup, testing.B, etc.): verified empirically that ruleguard resolves
// short names via its own stdlib import-path table, not literal string
// matching, so the short form is not a reliability risk.
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
//
// # Go 1.27 modernizers (harvested 2026-08-25, MEASURED against golangci-lint
// 2.13.1 on go 1.27.0)
//
// Go 1.27 added stdlib APIs and new go fix modernizers. Each candidate matcher
// was probed against this config's ENABLED stock analyzers (modernize,
// staticcheck) before adding, per the overlap policy above: a probe file
// planting every old pattern was run through golangci-lint, and a matcher was
// added only where no stock analyzer fired.
//
// Added (the probe reported zero modernize/staticcheck findings on each pattern,
// so this is unique coverage):
//
//   - random.go's RandMethodN: the generic Rand.N method (generic methods being
//     the Go 1.27 language feature); no modernizer suggests it.
//   - net.go's ResponseBodyDrain: Go 1.27's HTTP/1 auto-drain on Close makes an
//     explicit io.Copy(io.Discard, Body) redundant; no analyzer flags it
//     (errcheck fires only on an unchecked copy/close, a different concern).
//   - net.go's URLValuesClone: url.Values.Clone deep-copies where maps.Clone
//     shares the []string values; no analyzer flags the maps.Clone form.
//   - strings.go's CutLast: strings.CutLast/bytes.CutLast replace LastIndex
//     slicing; no analyzer suggests it.
//   - testing.go's SynctestSleep: synctest.Sleep folds time.Sleep+synctest.Wait.
//   - testing.go's HttptestNewTestServer: httptest.NewTestServer for a synctest
//     bubble; no analyzer is synctest-context aware.
//   - uuid.go's StdlibUUID: the new stdlib uuid package. Advisory and currently
//     INERT here (agy-mcp does not import github.com/google/uuid); kept for the
//     shared cross-project standard and to fire if that dependency is ever added.
//
// Declined (a stock analyzer this config already enables covers the pattern, so
// a matcher would only lose the uniq-by-line lottery, the exact drop rule above).
// Each was verified firing on the same probe:
//
//   - AtomicTypes (typed sync/atomic wrappers): modernize's atomictypes analyzer
//     (new in Go 1.27's go fix) already fires, with autofix.
//   - EmbeddedFieldLiteral (promoted-field struct-literal keys): modernize's
//     embedlit analyzer (new in Go 1.27's go fix) already fires, with autofix.
//   - tls.Config.Rand (deprecated in Go 1.27): staticcheck SA1019 already fires,
//     and its own message names testing/cryptotest.SetGlobalRandom, so a
//     richer-message matcher would add nothing.
//   - runtime.SetFinalizer -> AddCleanup: this package's own PreferAddCleanup
//     (runtime.go) already covers it; not a Go 1.27 change.
//   - sort.Ints/Strings/Float64s -> slices.Sort: this package's own SortToSlices
//     (slices.go) already covers it; not a Go 1.27 change.
package gorules
