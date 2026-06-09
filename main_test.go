package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tphakala/agy-mcp/internal/jobstore"
	"github.com/tphakala/agy-mcp/internal/testutil"
)

func jsonMarshalForTest(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// Build the binary once and use it as its own supervisor against a fake agy.
func TestRunJobSubcommandEndToEnd(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agy-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	agy := testutil.WriteFakeAgy(t, testutil.FakeAgy{Stdout: "E2E OK", Exit: 0})

	jobDir := t.TempDir()
	meta := jobstore.Meta{ID: filepath.Base(jobDir), AgyPath: agy, Args: []string{"-p", "x"}}
	b, _ := jsonMarshalForTest(meta)
	_ = os.WriteFile(filepath.Join(jobDir, "meta.json"), b, 0o644)

	cmd := exec.Command(bin, "run-job", jobDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run-job: %v\n%s", err, out)
	}
	out, _ := os.ReadFile(filepath.Join(jobDir, "out"))
	if strings.TrimSpace(string(out)) != "E2E OK" {
		t.Fatalf("out = %q", out)
	}
}
