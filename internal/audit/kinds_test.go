package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestAllKindsCoversEveryKindConstant scans the audit package source
// files for every `KindXxx Kind = "..."` constant declaration and
// asserts that the corresponding constant appears in AllKinds. Catches
// the easy mistake of adding a new Kind constant and forgetting to wire
// it into the registry — without that, every downstream lint test that
// iterates AllKinds is silently incomplete.
func TestAllKindsCoversEveryKindConstant(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse audit pkg: %v", err)
	}

	declared := map[string]Kind{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				gd, ok := n.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					return true
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					ident, ok := vs.Type.(*ast.Ident)
					if !ok || ident.Name != "Kind" {
						continue
					}
					if len(vs.Names) != 1 || len(vs.Values) != 1 {
						continue
					}
					lit, ok := vs.Values[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Errorf("unquote %s: %v", vs.Names[0].Name, err)
						continue
					}
					declared[vs.Names[0].Name] = Kind(val)
				}
				return true
			})
		}
	}

	if len(declared) == 0 {
		t.Fatal("found no Kind constants — parser miss?")
	}

	inAll := map[Kind]bool{}
	for _, k := range AllKinds {
		inAll[k] = true
	}

	for name, val := range declared {
		if !inAll[val] {
			t.Errorf("Kind constant %s (%q) declared in audit package but missing from AllKinds — add it to kinds.go", name, val)
		}
		if !IsRegistered(val) {
			t.Errorf("Kind constant %s (%q) not reported by IsRegistered", name, val)
		}
	}
}
