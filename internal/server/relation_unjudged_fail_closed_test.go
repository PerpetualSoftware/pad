package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3012. The server's relation passes used to settle an unjudged
// `wrong_collection` issue by RE-RESOLVING its value and checking visibility on
// what came back: a second lookup that a delete could race. Visibility now
// lives in the store resolver (the day-64 ruling), so an unjudged issue is a
// feeder that skipped the resolver's visibility argument, and the pass fails
// closed to `not_found` with no lookup.

// TestUnjudgedWrongCollectionFailsClosed hands the late pass an unjudged
// `wrong_collection` naming a LIVE item the requester (the workspace owner)
// CAN see. The old re-resolve would have found it, judged it visible and kept
// `wrong_collection`; so `not_found` here is only reachable without the second
// lookup. A judged issue in the same slice is left alone.
func TestUnjudgedWrongCollectionFailsClosed(t *testing.T) {
	f := newDoorFixture(t)
	r := httptest.NewRequest("POST", "/", nil)
	r = r.WithContext(WithCurrentUser(r.Context(), f.owner))

	issues := []store.RelationIssue{
		{Key: "owner_ref", Value: f.target.ID, Target: "elsewhere", Reason: store.RelationTargetWrongCollection},
		{Key: "judged", Value: f.target.ID, Target: "elsewhere", Reason: store.RelationTargetWrongCollection, VisibilityChecked: true},
	}
	if err := f.srv.collapseInvisibleRelationIssues(r, f.ws.ID, "owner", issues); err != nil {
		t.Fatalf("collapse: %v", err)
	}
	if issues[0].Reason != store.RelationTargetNotFound {
		t.Errorf("unjudged wrong_collection = %s, want %s: an issue no resolver judged must fail closed without a lookup",
			issues[0].Reason, store.RelationTargetNotFound)
	}
	if issues[1].Reason != store.RelationTargetWrongCollection {
		t.Errorf("judged wrong_collection = %s, want it left as %s: the resolver's verdict is trusted",
			issues[1].Reason, store.RelationTargetWrongCollection)
	}
}

// TestRelationFeedersPassTheRequesterVisibility is the guard the fail-closed
// pass relies on to stay unreachable. Every store function that takes a
// RelationVisibilityFunc, and the copy's RelationVisibility field, must be fed
// `relationVisibility(...)` from the server: a nil (or anything else) would
// hand the resolver no requester, and its issues would arrive unjudged.
//
// The function set is DERIVED from the store's own signatures, not listed
// here, so a new visibility-taking function joins the guard by existing. The
// shape asserted is structural (an argument that is a call to
// relationVisibility, or a local bound from one in the same function), not
// textual, so line breaks, argument order and an intermediate variable do not
// matter.
func TestRelationFeedersPassTheRequesterVisibility(t *testing.T) {
	fset := token.NewFileSet()

	// 1. Which store functions take a RelationVisibilityFunc?
	takesVisibility := map[string]bool{}
	storeFiles, err := filepath.Glob(filepath.Join("..", "store", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range storeFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() {
				continue
			}
			for _, p := range fn.Type.Params.List {
				if id, ok := p.Type.(*ast.Ident); ok && id.Name == "RelationVisibilityFunc" {
					takesVisibility[fn.Name.Name] = true
				}
			}
		}
	}
	// Precondition: the derivation found the functions this guard exists for.
	// An empty set would pass every check below vacuously.
	for _, must := range []string{"ResolveRelationReferents", "ResolveLateRelationDefaults", "MigrateRelationReferents"} {
		if !takesVisibility[must] {
			t.Fatalf("derivation did not find %s among visibility-taking store functions (found %v)", must, sortedVisibilityNames(takesVisibility))
		}
	}

	isVisibilityCall := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "relationVisibility"
	}

	// 2. Every server call to one of them, and every RelationVisibility field.
	serverFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	calls, fields := 0, 0
	for _, path := range serverFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		// A local bound from relationVisibility(...) in the same function counts
		// as the call itself, so storing it in a variable first is not flagged.
		// Scoped per function: the set is rebuilt at every FuncDecl.
		boundVis := map[string]bool{}
		fedBy := func(e ast.Expr) bool {
			if isVisibilityCall(e) {
				return true
			}
			id, ok := e.(*ast.Ident)
			return ok && boundVis[id.Name]
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				boundVis = map[string]bool{}
				if x.Body != nil {
					ast.Inspect(x.Body, func(m ast.Node) bool {
						if as, ok := m.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
							for i, rhs := range as.Rhs {
								if id, ok := as.Lhs[i].(*ast.Ident); ok && isVisibilityCall(rhs) {
									boundVis[id.Name] = true
								}
							}
						}
						return true
					})
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || !takesVisibility[sel.Sel.Name] {
					return true
				}
				calls++
				fed := false
				for _, a := range x.Args {
					if fedBy(a) {
						fed = true
					}
				}
				if !fed {
					t.Errorf("%s: %s is not passed relationVisibility(...); its issues would reach a caller unjudged",
						fset.Position(x.Pos()), sel.Sel.Name)
				}
			case *ast.KeyValueExpr:
				if k, ok := x.Key.(*ast.Ident); ok && k.Name == "RelationVisibility" {
					fields++
					if !fedBy(x.Value) {
						t.Errorf("%s: RelationVisibility is not set to relationVisibility(...)", fset.Position(x.Pos()))
					}
				}
			}
			return true
		})
	}
	// Precondition: the scan reached the feeders at all.
	if calls < 5 || fields < 1 {
		t.Fatalf("scan found %d feeder calls and %d RelationVisibility fields; expected at least 5 and 1 — the instrument is not looking where the feeders are", calls, fields)
	}
}

func sortedVisibilityNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
