//go:build linux || darwin

package manager

import (
	"testing"

	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// TestDoctorHealthy: a resolvable, current, reachable agy and a writable state
// dir yield an all-passing report that exits zero. It execs the fake agy for the
// model listing (the auth/reachability check), so it is posix-gated like the
// other WriteFakeAgy tests.
func TestDoctorHealthy(t *testing.T) {
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{
		Models: []testutil.FakeModel{{ID: "gemini-3.1-pro-high", Label: "Gemini 3.1 Pro (High)"}},
	})
	m := newManager(t, managerOpts{agyPath: agy})

	report := m.Doctor(t.Context())
	if !report.OK() {
		for _, c := range report.Checks {
			t.Logf("[%s] %s: %s", c.Status, c.Name, c.Detail)
		}
		t.Fatal("Doctor should report OK for a healthy install")
	}
	// The reachability check must actually have listed the fake's model, proving it
	// execed agy rather than passing vacuously.
	reach := findCheck(t, report, checkAgyReachableName)
	if reach.Status != CheckPass {
		t.Errorf("reachability check = %v (%s), want PASS", reach.Status, reach.Detail)
	}
}
