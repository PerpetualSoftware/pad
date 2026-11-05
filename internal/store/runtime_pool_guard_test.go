package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TASK-4278: BoundRuntimePool replaces the store's *sql.DB once, at boot. A
// struct that cached the handle from Store.DB() BEFORE the swap would keep
// querying the closed startup pool. Outside tests, the only structs allowed to
// hold a *sql.DB field are the store itself and the DB-stats collector, which
// the server registers AFTER the swap (cmd/pad/cmd_server.go; the ordering is
// pinned by TestBoundRuntimePoolSwapPrecedesEverythingStarted).
func TestDBHandleNotCaptured(t *testing.T) {
	t.Parallel()
	allowed := map[string]string{
		"store/store.go":     "Store",
		"metrics/metrics.go": "dbStatsCollector",
	}
	var found []string
	for _, root := range []string{"..", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.Contains(filepath.ToSlash(path), "/testdata/") {
				return nil
			}
			f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					return true
				}
				for _, fld := range st.Fields.List {
					star, ok := fld.Type.(*ast.StarExpr)
					if !ok {
						continue
					}
					sel, ok := star.X.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "DB" {
						continue
					}
					if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "sql" {
						continue
					}
					rel := filepath.ToSlash(path)
					rel = strings.TrimPrefix(strings.TrimPrefix(rel, "../../"), "../")
					if allowed[rel] != ts.Name.Name {
						found = append(found, rel+": "+ts.Name.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range found {
		t.Errorf("struct holds a *sql.DB field: %s (it would outlive the BoundRuntimePool swap; take the handle from the store per call)", f)
	}
}
