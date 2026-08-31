package manager

import (
	"context"
	"testing"

	"golang.org/x/sys/windows"
)

// TestProbeCmdSuppressesConsoleWindow pins the binding #158 relies on: every
// command the manager builds through probeCmd carries CREATE_NO_WINDOW, so the
// short-lived probes do not flash a console window. Deleting the ConfigureNoWindow
// call inside probeCmd must turn this red by assertion, not by a nil panic (see
// the nil guard below), which is why the helper exists in one inspectable place.
func TestProbeCmdSuppressesConsoleWindow(t *testing.T) {
	cmd := probeCmd(context.Background(), "cmd.exe", "/c", "exit")
	if cmd.SysProcAttr == nil {
		t.Fatal("probeCmd must set SysProcAttr so window suppression is applied")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Error("probeCmd must set CREATE_NO_WINDOW")
	}
}

// TestProbeCmdDoesNotGroupOrDetach: the probes want window suppression alone,
// not the process-group or detachment flags the supervisor path uses. Asserting
// their absence keeps probeCmd from quietly becoming an alias for ConfigureGroup
// or StartDetached.
func TestProbeCmdDoesNotGroupOrDetach(t *testing.T) {
	cmd := probeCmd(context.Background(), "cmd.exe", "/c", "exit")
	if cmd.SysProcAttr == nil {
		t.Fatal("probeCmd must set SysProcAttr")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP != 0 {
		t.Error("probeCmd must not set CREATE_NEW_PROCESS_GROUP")
	}
	if cmd.SysProcAttr.CreationFlags&windows.DETACHED_PROCESS != 0 {
		t.Error("probeCmd must not detach the child")
	}
}
