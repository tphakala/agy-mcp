//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// BenchmarkLoop detects the old benchmark iteration pattern and suggests using b.Loop().
//
// The old pattern:
//
//	func BenchmarkFoo(b *testing.B) {
//	    for i := 0; i < b.N; i++ {
//	        // work
//	    }
//	}
//
// New pattern (Go 1.24+):
//
//	func BenchmarkFoo(b *testing.B) {
//	    for b.Loop() {
//	        // work
//	    }
//	}
//
// Benefits:
//   - Setup/cleanup executes only once per -count
//   - Compiler cannot optimize away the loop body
//   - Cleaner, more idiomatic code
//
// See: https://pkg.go.dev/testing#B.Loop
func BenchmarkLoop(m dsl.Matcher) {
	// Pattern 1: for i := 0; i < b.N; i++
	// No auto-fix: loop variable $i may be used in body
	m.Match(
		`for $i := 0; $i < $b.N; $i++ { $*body }`,
	).
		Where(m["b"].Type.Is("*testing.B")).
		Report("use for $b.Loop() { ... } instead of for $i := 0; $i < $b.N; $i++ (Go 1.24+); if using $i in body, declare it separately")

	// Pattern 2: for i := range b.N (Go 1.22+ style)
	// No auto-fix: loop variable $i may be used in body
	m.Match(
		`for $i := range $b.N { $*body }`,
	).
		Where(m["b"].Type.Is("*testing.B")).
		Report("use for $b.Loop() { ... } instead of for $i := range $b.N (Go 1.24+); if using $i in body, declare it separately")

	// Pattern 3: for range b.N (no variable) - safe for auto-fix
	m.Match(
		`for range $b.N { $*body }`,
	).
		Where(m["b"].Type.Is("*testing.B")).
		Report("use for $b.Loop() { ... } instead of for range $b.N (Go 1.24+)").
		Suggest("for $b.Loop() { $body }")
}

// TestingArtifactDir detects os.MkdirTemp in test files and suggests using
// the testing.T.ArtifactDir method added in Go 1.26.
//
// Old pattern:
//
//	func TestFoo(t *testing.T) {
//	    dir, err := os.MkdirTemp("", "test-output-*")
//	    if err != nil { t.Fatal(err) }
//	    defer os.RemoveAll(dir)
//	    // write test artifacts to dir
//	}
//
// New pattern (Go 1.26+):
//
//	func TestFoo(t *testing.T) {
//	    dir := t.ArtifactDir()
//	    // write test artifacts to dir
//	    // directory persists after test for inspection
//	}
//
// Benefits:
//   - No error handling needed
//   - Automatically named after the test
//   - Survives test cleanup (unlike t.TempDir)
//   - Location reported with -artifacts flag
//   - Consistent output location across test runs
//
// Note: ArtifactDir is for test output files (golden files, debug output,
// snapshots), not for temporary scratch space. If you need a directory that
// is cleaned up after the test, continue using t.TempDir().
//
// See: https://pkg.go.dev/testing#T.ArtifactDir
func TestingArtifactDir(m dsl.Matcher) {
	// os.MkdirTemp in test files - advisory suggestion
	m.Match(
		`os.MkdirTemp($dir, $pattern)`,
	).
		Where(m.File().Name.Matches(`_test\.go$`)).
		Report("in tests, consider t.ArtifactDir() for test output files instead of os.MkdirTemp (Go 1.26+); use t.TempDir() for scratch space that should be cleaned up")
}

// SynctestSleep detects time.Sleep immediately followed by synctest.Wait and
// suggests the synctest.Sleep helper added in Go 1.27.
//
// Old pattern:
//
//	time.Sleep(d)
//	synctest.Wait()
//
// New pattern (Go 1.27+):
//
//	synctest.Sleep(d)
//
// synctest.Sleep is documented as exactly equivalent to the two-call sequence:
// it advances the bubble's fake clock by d and then waits for every other
// goroutine in the bubble to be durably blocked, so the system under test has
// settled before the test continues.
//
// Report-only, no autofix. Rewriting to synctest.Sleep($d) drops the only
// time.Sleep call, so when $d does not itself reference time and the file has no
// other time.* use, --fix would orphan the "time" import and fail to compile
// (ruleguard does not prune imports). This matcher advises the swap rather than
// performing it.
//
// See: https://pkg.go.dev/testing/synctest#Sleep
func SynctestSleep(m dsl.Matcher) {
	m.Match(
		`time.Sleep($d); synctest.Wait()`,
	).
		Report("use synctest.Sleep($d) instead of time.Sleep($d) followed by synctest.Wait() (Go 1.27+)")
}

// HttptestNewTestServer detects httptest.NewServer started inside a synctest
// bubble and suggests httptest.NewTestServer, added in Go 1.27.
//
// Old pattern:
//
//	srv := httptest.NewServer(handler)
//	defer srv.Close()
//
// New pattern (Go 1.27+):
//
//	srv := httptest.NewTestServer(t, handler)
//
// NewTestServer takes a testing.TB and serves over an in-memory network by
// default, which is what a synctest bubble needs: a real loopback listener has
// goroutines outside the bubble, so the bubble never reaches a durably blocked
// state. The httptest.Server docs now say most tests should use NewTestServer,
// but the in-memory network only serves requests sent through srv.Client(), so
// this rule fires only on a synctest.Test call whose closure body starts an
// httptest.NewServer, where the real network is an actual problem rather than
// a style choice; a NewServer elsewhere in the same file is left alone, and a
// NewServer started by a helper the closure calls is not seen. NewTLSServer is
// not matched: NewTestServer's URL is plain http://example.com, so a TLS test
// needs its own HTTPS URL and is not a mechanical swap.
//
// See: https://pkg.go.dev/net/http/httptest#NewTestServer
func HttptestNewTestServer(m dsl.Matcher) {
	m.Match(
		`synctest.Test($_, func($*_) { $*body })`,
	).
		Where(m["body"].Contains(`httptest.NewServer($_)`)).
		Report("this synctest bubble starts an httptest.NewServer on a real listener; pass the enclosing test's *testing.T to httptest.NewTestServer (with the handler) instead, which serves over an in-memory network that stays inside the bubble, reachable only through srv.Client() (Go 1.27+)")
}
