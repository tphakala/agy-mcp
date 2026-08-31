package manager

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The console-window suppression #158 added is only real if every agy spawn in
// this package goes through probeCmd, which is where proc.ConfigureNoWindow is
// applied. probeCmd's own Windows test pins that the helper sets the flag; this
// pins that the probes actually use the helper.
//
// Without it the binding is invisible to the suite: replacing probeCmd with a
// bare exec.CommandContext at both production call sites leaves every test, go
// vet, and the Windows vet green, so the fix can be reverted silently. That is a
// property of the call sites rather than of any single function's behaviour,
// which is why it is checked structurally instead of by an assertion on output.
//
// It runs on every platform, unlike probe_windows_test.go, so a POSIX-only CI
// leg still catches a regression here.
func TestNoDirectExecOutsideProbeCmd(t *testing.T) {
	const helper = "probeCmd"

	// The supervisor spawn is the one legitimate direct exec in this package: it
	// starts agy-mcp's own supervisor, not agy, and it goes through
	// proc.StartDetached, whose DETACHED_PROCESS leaves the child with no console
	// at all (CREATE_NO_WINDOW is ignored alongside it, as ConfigureGroup's doc
	// records). The exemption is keyed on the enclosing function so that renaming
	// or moving that spawn fails this test rather than silently widening the
	// exemption; the positive control below proves the entry still matches
	// something.
	exempt := map[string]string{"StartJob": "spawns the supervisor via proc.StartDetached, which drops the console"}
	seenExempt := map[string]bool{}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var checked int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++

		// Walk each top-level function separately so the enclosing function's name
		// is known at the call site, which is what lets probeCmd itself be exempt.
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Name.Name == helper {
				continue
			}
			if _, ok := exempt[fn.Name.Name]; ok {
				seenExempt[fn.Name.Name] = true
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "exec" {
					return true
				}
				if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
					return true
				}
				t.Errorf("%s: %s calls exec.%s directly; agy spawns in this package must go through %s so proc.ConfigureNoWindow is applied (see #158)",
					fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name, helper)
				return true
			})
		}
	}

	// Positive control: a glob that matched nothing, or a package that stopped
	// containing the helper, would make the loop above vacuous.
	if checked == 0 {
		t.Fatal("parsed no non-test Go files; the check above proved nothing")
	}
	if !strings.Contains(readSource(t, "manager.go"), "func "+helper+"(") {
		t.Fatalf("%s is not defined in manager.go; this test is pinned to the wrong helper", helper)
	}
	// An exemption that no longer matches any function is a stale carve-out, and a
	// stale carve-out silently permits whatever later takes that name.
	for name, why := range exempt {
		if !seenExempt[name] {
			t.Errorf("exemption for %s (%s) matched no function; remove it or update the name", name, why)
		}
	}
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
