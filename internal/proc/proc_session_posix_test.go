//go:build linux || darwin

package proc

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// TestConfigureSessionDetachesFromControllingTerminal is the mechanism-level guard
// for the SIGTTOU stop bug. The fix is that the agy child leads a NEW session,
// which by definition has no controlling terminal. A ConfigureSession child must
// therefore be its own session leader (getsid == pid) and must not share the
// parent's session; a ConfigureGroup child (Setpgid only) stays in the parent's
// session and keeps its controlling terminal. Without a controlling terminal,
// agy's startup tcsetattr on /dev/tty returns ENXIO instead of raising SIGTTOU and
// stopping the process.
func TestConfigureSessionDetachesFromControllingTerminal(t *testing.T) {
	parentSID, err := unix.Getsid(0)
	if err != nil {
		t.Fatalf("Getsid(self): %v", err)
	}

	start := func(configure func(*exec.Cmd)) *exec.Cmd {
		cmd := exec.Command("sleep", "30")
		configure(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		return cmd
	}

	session := start(ConfigureSession)
	sid, err := unix.Getsid(session.Process.Pid)
	if err != nil {
		t.Fatalf("Getsid(session child): %v", err)
	}
	if sid != session.Process.Pid {
		t.Errorf("ConfigureSession child sid=%d, want it to lead its own session (pid=%d)", sid, session.Process.Pid)
	}
	if sid == parentSID {
		t.Errorf("ConfigureSession child must not share the parent's session %d (it would keep the controlling terminal)", parentSID)
	}

	group := start(ConfigureGroup)
	gsid, err := unix.Getsid(group.Process.Pid)
	if err != nil {
		t.Fatalf("Getsid(group child): %v", err)
	}
	if gsid != parentSID {
		t.Errorf("ConfigureGroup child sid=%d, want the parent's session %d (Setpgid keeps the session and its controlling terminal)", gsid, parentSID)
	}
}
