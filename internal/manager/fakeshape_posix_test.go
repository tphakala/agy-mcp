//go:build linux || darwin

package manager

import (
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
	"github.com/tphakala/agy-mcp/v2/internal/testutil"
)

// TestFakeSupervisorStagesBothJobDirShapes pins the fake supervisor's ability to
// produce EITHER on-disk shape the manager has to classify.
//
// The real supervisor writes a progress marker before exec, but only for a run
// it drives through stream-json, so both a marked and an unmarked job dir are
// things production can produce. A fake that could only stage the marked one
// would leave the manager's legacy branch unreachable through a spawned
// supervisor, which is how the marker's own default came to be untested in the
// first place. Asserting both here is what keeps the opt-out honest: a default
// that quietly stopped writing, or an opt-out that quietly started, fails this.
func TestFakeSupervisorStagesBothJobDirShapes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		omit       bool
		wantMarker bool
	}{
		{"default writes the marker, as the real supervisor does", false, true},
		{"opt-out stages the shape a legacy or non-stream job leaves", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// agyPath is set explicitly, as every sibling StartJob test does:
			// leaving it empty makes StartJob resolve "agy" on PATH, which passes
			// on a developer machine that has agy installed and fails on CI, which
			// does not. Nothing here execs it; the fake supervisor stands in.
			m := newManager(t, managerOpts{
				agyPath: "/usr/bin/agy",
				supervisorExe: testutil.WriteFakeSupervisor(t, testutil.FakeSupervisor{
					Out: "done", OmitProgressMarker: tc.omit,
				}),
			})
			job, err := m.StartJob(StartRequest{Prompt: "x", Cwd: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			testutil.WaitFor(t, 10*time.Second, func() bool {
				_, ok := m.store.ExitCode(job.ID)
				return ok
			}, "job to reach its exit-code sentinel")

			dir, err := m.store.Dir(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := jobstore.ReadProgressDir(dir); ok != tc.wantMarker {
				t.Fatalf("progress marker present = %v, want %v", ok, tc.wantMarker)
			}
		})
	}
}
