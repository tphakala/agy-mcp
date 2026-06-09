package manager

import (
	"os"
	"testing"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/jobstore"
)

func TestExpectedComm(t *testing.T) {
	if got := expectedComm("/usr/local/bin/agy-mcp"); got != "agy-mcp" {
		t.Errorf("expectedComm basename = %q, want agy-mcp", got)
	}
	// Kernel /proc/comm is capped at 15 chars; a longer basename is truncated.
	if got := expectedComm("/x/agy-mcp-v1.2.3-linux-amd64"); got != "agy-mcp-v1.2.3-" {
		t.Errorf("expectedComm truncation = %q (len %d)", got, len(got))
	}
}

func TestParseStartTimeTicks(t *testing.T) {
	// comm is wrapped in a single outer paren pair but may itself contain spaces
	// and parens; parsing must key off the LAST ')'. starttime is field 22, which
	// is index 19 of the whitespace-split remainder after that ')'.
	const stat = "1234 (odd) proc (x) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 8888888 0 0 0 0"
	got, ok := parseStartTimeTicks([]byte(stat))
	if !ok || got != 8888888 {
		t.Fatalf("parseStartTimeTicks = %d,%v, want 8888888,true", got, ok)
	}

	// Malformed input returns (0, false), never a misread.
	for _, bad := range []string{"", "no parens here", "123 (x) S 1 2 3"} {
		if v, ok := parseStartTimeTicks([]byte(bad)); ok || v != 0 {
			t.Errorf("parseStartTimeTicks(%q) = %d,%v, want 0,false", bad, v, ok)
		}
	}
}

func TestProcessAliveStartTimeCheck(t *testing.T) {
	pid, exePath := startFakeLiveSupervisor(t)
	m := New(config.Config{SupervisorExe: exePath, StateDir: t.TempDir(), MaxConcurrency: 4})

	actual, ok := readStartTimeTicks(pid)
	if !ok {
		t.Fatalf("could not read start time for live pid %d", pid)
	}
	base := jobstore.Meta{PID: pid, BootID: readBootID()}

	// Recorded start time matches the live process: alive.
	match := base
	match.StartTimeTicks = actual
	if !m.processAlive(match) {
		t.Error("matching start time should read as alive")
	}

	// Recorded start time differs (a same-boot recycled pid now owned by another
	// agy-mcp would pass the comm check but fail here): not our process.
	mismatch := base
	mismatch.StartTimeTicks = actual + 1
	if m.processAlive(mismatch) {
		t.Error("mismatched start time should read as not alive")
	}

	// A matching (boot id, pid, starttime) triple pins the exact process, so it
	// must read as alive even when the comm does not match the supervisor name.
	// This is the fork-to-exec window: comm is inherited from the parent (or is
	// briefly the script interpreter's) before exec sets the final name.
	wrongComm := New(config.Config{SupervisorExe: "/bogus/not-this-process", StateDir: t.TempDir(), MaxConcurrency: 4})
	if !wrongComm.processAlive(match) {
		t.Error("matching start time should be authoritative over a mismatched comm")
	}

	// Unknown (0) start time: the check is skipped, preserving prior behavior.
	unknown := base
	unknown.StartTimeTicks = 0
	if !m.processAlive(unknown) {
		t.Error("zero start time should skip the check and read as alive")
	}
}

func TestReadStartTimeTicks(t *testing.T) {
	// Our own pid has a readable, non-zero start time.
	got, ok := readStartTimeTicks(os.Getpid())
	if !ok || got == 0 {
		t.Fatalf("readStartTimeTicks(self) = %d,%v, want non-zero,true", got, ok)
	}
	// A pid that cannot exist yields (0, false), not a panic or a misread.
	if v, ok := readStartTimeTicks(1 << 30); ok || v != 0 {
		t.Errorf("readStartTimeTicks(bogus) = %d,%v, want 0,false", v, ok)
	}
}
