package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// BUG-4878: every real API server a test in this package builds must be
// stopped. Server.Stop drains the goroutines it starts with goAsync (the
// activity touch after each authenticated request, among others), and
// httptest.Server.Close does not wait for them. One that outlives its test
// calls time.Now, which reads time.Local, while a later test's tzCLIWithLocal
// writes time.Local: a data race that fails the package under -race, blamed on
// whichever test happens to be running.
//
// The rule checked: a function that calls New from internal/server also
// mentions Stop (t.Cleanup(srv.Stop), registered before any httptest
// server's Close so it runs after it, or an explicit srv.Stop()).
func TestBUG4878_EveryTestServerIsStopped(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	const serverPkg = "github.com/PerpetualSoftware/pad/internal/server"
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		alias := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == serverPkg {
				alias = "server"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
			}
		}
		if alias == "" {
			continue
		}
		// Walk with a stack of enclosing function bodies so each New call is
		// checked against the innermost function that contains it.
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "New" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); !ok || id.Name != alias {
				return true
			}
			checked++
			var body *ast.BlockStmt
			for i := len(stack) - 1; i >= 0 && body == nil; i-- {
				switch fn := stack[i].(type) {
				case *ast.FuncDecl:
					body = fn.Body
				case *ast.FuncLit:
					body = fn.Body
				}
			}
			if body == nil || !mentionsStop(body) {
				t.Errorf("%s: %s.New is never stopped in its function; add t.Cleanup(srv.Stop) before the httptest server's Close", fset.Position(call.Pos()), alias)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no internal/server New calls; the guard is checking nothing")
	}
}

func mentionsStop(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Stop" {
			found = true
		}
		return !found
	})
	return found
}
