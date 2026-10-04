package mcptools

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// failureReasons returns the value of every Reason* constant declared in
// internal/manager/status.go. It parses the source rather than listing the
// values, so a constant added later is picked up.
func failureReasons(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../manager/status.go", nil, 0)
	if err != nil {
		t.Fatalf("parse status.go: %v", err)
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Reason") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				out = append(out, v)
			}
		}
	}
	return out
}

// TestFailureReasonSetDocumented keeps the closed failure_reason set from
// drifting between status.go, the FailureReason schema text and the README: every
// Reason* constant must be named in both documents.
func TestFailureReasonSetDocumented(t *testing.T) {
	reasons := failureReasons(t)
	if len(reasons) == 0 {
		t.Fatal("found no Reason constants in status.go")
	}
	field, ok := reflect.TypeFor[statusOutput]().FieldByName("FailureReason")
	if !ok {
		t.Fatal("statusOutput has no FailureReason field")
	}
	schema := field.Tag.Get("jsonschema")
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	var paragraph string
	for line := range strings.Lines(string(readme)) {
		if strings.HasPrefix(line, "`failure_reason` classifies") {
			paragraph = line
			break
		}
	}
	if paragraph == "" {
		t.Fatal("README has no paragraph starting \"`failure_reason` classifies\"")
	}
	for _, r := range reasons {
		// Bare words like timeout and unknown also occur in ordinary prose, so
		// require the form each document uses to list a value: "name (" in the
		// schema text and a backticked name in the README.
		if !strings.Contains(schema, r+" (") {
			t.Errorf("failure_reason %q is missing from the FailureReason schema text", r)
		}
		if !strings.Contains(paragraph, "`"+r+"`") {
			t.Errorf("failure_reason %q is missing from the README failure_reason paragraph", r)
		}
	}
}
