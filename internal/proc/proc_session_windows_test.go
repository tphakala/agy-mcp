package proc

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// TestConfigureSessionSetsProcessGroupAndNoWindow: Windows has no POSIX session or
// SIGTTOU, so the controlling-terminal fix is a no-op concept here; the agy child
// still needs its own process group and no console window, exactly what
// ConfigureGroup provides. ConfigureSession must therefore set the same flags.
func TestConfigureSessionSetsProcessGroupAndNoWindow(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	ConfigureSession(cmd)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatal("ConfigureSession must set CREATE_NEW_PROCESS_GROUP")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("ConfigureSession must set CREATE_NO_WINDOW")
	}
}

// TestConfigureSessionPreservesExistingFlags: ConfigureSession must OR its flags
// into any CreationFlags a caller set first rather than overwrite them.
func TestConfigureSessionPreservesExistingFlags(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_DEFAULT_ERROR_MODE}
	ConfigureSession(cmd)
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_DEFAULT_ERROR_MODE == 0 {
		t.Error("ConfigureSession must preserve a pre-existing CreationFlag")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("ConfigureSession must add CREATE_NEW_PROCESS_GROUP")
	}
}
