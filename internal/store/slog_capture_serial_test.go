package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestSlogCaptureTestsAreNotParallel is BUG-3088's class guard. A test that
// installs a capturing handler as the PROCESS-GLOBAL slog default shares that
// default with every test running at the same moment. Under t.Parallel() a
// sibling's log lines land in the capture, and two capturing tests steal each
// other's records: TestImportRoutingIgnoresSoftDeletedCollections failed 13
// times in 40 on a sibling import's legitimate warning. Non-parallel top-level
// tests never overlap parallel ones, so the rule is simply: a test that calls
// slog.SetDefault does not call t.Parallel().
func TestSlogCaptureTestsAreNotParallel(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	capturing := 0
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			var setsDefault, parallel bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "slog" && sel.Sel.Name == "SetDefault" {
					setsDefault = true
				}
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "t" && sel.Sel.Name == "Parallel" {
					parallel = true
				}
				return true
			})
			if setsDefault {
				capturing++
				if parallel {
					t.Errorf("%s: %s swaps the global slog default and calls t.Parallel(); a parallel sibling's logs will reach its capture (BUG-3088)",
						fset.Position(fn.Pos()), fn.Name.Name)
				}
			}
		}
	}
	// Precondition: the scan found the capturing tests it exists to police.
	if capturing < 4 {
		t.Fatalf("found %d tests that call slog.SetDefault, expected at least 4; the scan is not looking where they are", capturing)
	}
}
