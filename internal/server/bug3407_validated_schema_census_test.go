package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3407 census (lead ruling, day 86, condition 3). Every HTTP path that
// validates item fields and then writes them must hand the store the schema
// bytes it validated against, so the store can re-check under its lock when
// the schema moved in between:
//   - a create or update call sits in a function that sets ValidatedSchema;
//   - a move passes store.WithValidatedSchema.
// A call that writes no fields is exempt, with its reason. A new call site
// fails here until it is wired or exempted; an exemption that no longer
// matches a call, or whose function now sets ValidatedSchema, fails too.
//
// MCP and the CLI reach the store only through these HTTP handlers (the
// remote MCP door dispatches in-process to them; stdio and the CLI call the
// HTTP API), so this census covers them.

var bug3407StoreWrites = map[string]bool{
	"CreateItem": true, "UpdateItem": true, "UpdateItemWithPreCheck": true,
	"UpdateItemWithParentLink": true, "MoveItemWithPreCheck": true,
}

// file::function -> how many store writes it makes, and why none of them
// needs ValidatedSchema. The count pins the exemption to the calls that were
// reviewed: a new store write added to an exempt function fails the census.
type bug3407Exemption struct {
	calls int
	why   string
}

var bug3407Exempt = map[string]bug3407Exemption{
	"handlers_items_bulk.go::applyBulkOp":                 {1, "bulk assign: assignment columns only, no fields"},
	"handlers_items_bulk.go::bulkTagUpdate":               {1, "bulk tag/untag: tags only, no fields"},
	"handlers_items_content_route.go::routeContentUpdate": {1, "passes the update handler's own input, which carries ValidatedSchema"},
	"handlers_items_content_route.go::applierFirstWrite":  {1, "passes the update handler's own input (rowInput := *input)"},
	"handlers_item_versions.go::handleRestoreItemVersion": {2, "version restore: content only, no fields"},
}

func TestBug3407_EveryFieldWritingHandlerPassesTheValidatedSchema(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	seen := map[string]int{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := file + "::" + fn.Name.Name
			setsSchema := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if x.Sel.Name == "ValidatedSchema" {
						setsSchema = true
					}
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok && id.Name == "ValidatedSchema" {
						setsSchema = true
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !bug3407StoreWrites[sel.Sel.Name] {
					return true
				}
				recv, ok := sel.X.(*ast.SelectorExpr)
				if !ok || recv.Sel.Name != "store" {
					return true
				}
				pos := fset.Position(call.Pos())
				where := key + " @" + strconv.Itoa(pos.Line)
				if ex, ok := bug3407Exempt[key]; ok {
					seen[key]++
					if setsSchema {
						problems = append(problems, where+": exempt ("+ex.why+") but now sets ValidatedSchema; remove the exemption")
					}
					return true
				}
				if sel.Sel.Name == "MoveItemWithPreCheck" {
					if !callPassesValidatedSchemaOption(call) {
						problems = append(problems, where+": a move without store.WithValidatedSchema")
					}
					return true
				}
				if !setsSchema {
					problems = append(problems, where+": "+sel.Sel.Name+" in a function that never sets ValidatedSchema")
				}
				return true
			})
		}
	}
	for key, ex := range bug3407Exempt {
		if seen[key] != ex.calls {
			problems = append(problems, key+": exempt for "+strconv.Itoa(ex.calls)+" store write(s), found "+strconv.Itoa(seen[key])+"; review the new call or remove the exemption")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("BUG-3407 census:\n  %s", strings.Join(problems, "\n  "))
	}
}

func callPassesValidatedSchemaOption(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		c, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "WithValidatedSchema" {
			return true
		}
	}
	return false
}

// End to end (lead condition 4): a PATCH through the real update handler,
// with the schema changed after the handler validated and before the store's
// lock, is refused; the same window with a schema the value still satisfies
// lets it through.
func TestBug3407_AnHTTPPatchRacingASchemaChangeIsRefused(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)
	base := `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"}]}`
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]any{"name": "Things", "schema": base})
	if rr.Code != http.StatusCreated {
		t.Fatalf("collection: %d %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll.Slug+"/items", map[string]any{"title": "One"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	race := func(schema string) *httptest.ResponseRecorder {
		restore := srv.store.SetAfterItemPreLockReadHookForTesting(func(string) {
			s := schema
			if _, err := srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &s}); err != nil {
				t.Errorf("schema change: %v", err)
			}
		})
		defer restore()
		return doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, map[string]any{"fields_patch": map[string]any{"color": "blue"}})
	}

	forbids := `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"},{"key":"color","label":"Color","type":"select","options":["red"]}]}`
	if rr := race(forbids); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "schema changed") {
		t.Errorf("racing a schema that forbids the value: %d %s, want 400 naming the schema change", rr.Code, rr.Body.String())
	}
	got, _ := srv.store.GetItem(item.ID)
	if strings.Contains(got.Fields, "blue") {
		t.Errorf("fields = %s; nothing should have been written", got.Fields)
	}

	// Reset to the base schema, then race one the value still satisfies.
	b := base
	if _, err := srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &b}); err != nil {
		t.Fatal(err)
	}
	allows := `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"},{"key":"color","label":"Color","type":"select","options":["red","blue"]}]}`
	if rr := race(allows); rr.Code != http.StatusOK {
		t.Errorf("racing a schema the value satisfies: %d %s, want 200", rr.Code, rr.Body.String())
	}
}

// codex r3: the collaborative-content orderings answer through
// writeTypedItemRefusal, which needs its own 400 arm for the refusal.
func TestBug3407_TheContentRouteRefusalHelperAnswersTheSchemaRefusalWith400(t *testing.T) {
	srv := testServer(t)
	item := &models.Item{ID: "item-1", Ref: "TASK-1", Slug: "task-1"}
	rec := httptest.NewRecorder()
	err := fmt.Errorf("update item: %w", &store.ValidationError{Reason: "the collection's schema changed while this write was in flight: x"})
	if !srv.writeTypedItemRefusal(rec, item, err, false) {
		t.Fatal("not recognised as a refusal")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "validation_error") || !strings.Contains(rec.Body.String(), "schema changed") {
		t.Errorf("got %d %s; want 400 validation_error with the reason", rec.Code, rec.Body.String())
	}
}
