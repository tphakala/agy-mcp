package testutil

import (
	"os/exec"
	"strings"
	"testing"
)

func TestFakeAgyEmitsStdoutAndExit(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stdout: "hello world", Exit: 0})
	out, err := exec.Command(path, "-p", "ignored").Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello world" {
		t.Fatalf("stdout = %q, want %q", got, "hello world")
	}
}

func TestFakeAgyNonZeroExit(t *testing.T) {
	path := WriteFakeAgy(t, FakeAgy{Stderr: "boom", Exit: 3})
	err := exec.Command(path).Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 3 {
		t.Fatalf("exit = %v, want code 3", err)
	}
}
