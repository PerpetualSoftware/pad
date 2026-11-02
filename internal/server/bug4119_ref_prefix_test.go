package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestBUG4119_HTTPWrongPrefix(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, exists := range []bool{false, true} {
			for _, method := range []string{"POST", "PATCH", "DELETE"} {
				t.Run(fmt.Sprintf("pg=%t/ident=%t/%s", pg, exists, method), func(t *testing.T) {
					f := storetest.NewRefPrefixFixture(t, pg, exists)
					srv := New(f.Store)
					t.Cleanup(srv.Stop)
					path := "/api/v1/workspaces/" + f.Workspace.Slug + "/items/" + f.WrongRef
					body := map[string]any{"content": "Wrong write", "fields": map[string]any{}}
					if method == "POST" {
						path += "/comments"
						body = map[string]any{"body": "Wrong comment"}
					}
					before := f.Snapshot(t)
					rr := doRequest(srv, method, path, body)
					if rr.Code != http.StatusNotFound {
						t.Errorf("%s wrong-prefix: got %d, want 404", method, rr.Code)
					}
					f.AssertUnchanged(t, before)
				})
			}
		}
	}
}

func TestBUG4119_HTTPImportedUppercaseRefSlug(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, method := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
			t.Run(fmt.Sprintf("pg=%t/%s", pg, method), func(t *testing.T) {
				f, slugged := storetest.NewImportedUppercaseRefSlugFixture(t, pg)
				srv := New(f.Store)
				t.Cleanup(srv.Stop)
				slugOwner := f
				slugOwner.Item = slugged
				beforeNumber, beforeSlug := f.Snapshot(t), slugOwner.Snapshot(t)
				var body any
				if method == http.MethodPatch {
					body = map[string]string{"content": "Wrong write to uppercase slug owner"}
				}
				rr := doRequest(srv, method, "/api/v1/workspaces/"+f.Workspace.Slug+"/items/"+f.WrongRef, body)
				if rr.Code != http.StatusNotFound {
					t.Errorf("%s %s: got %d, want 404", method, f.WrongRef, rr.Code)
				}
				f.AssertUnchanged(t, beforeNumber)
				slugOwner.AssertUnchanged(t, beforeSlug)
			})
		}
	}
}

func TestBUG4119_HTTPMovedRefAndTerminalMiss(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprintf("pg=%t", pg), func(t *testing.T) {
			f := storetest.NewRefPrefixFixture(t, pg, false)
			srv := New(f.Store)
			t.Cleanup(srv.Stop)
			plan, err := f.Store.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Plans", Prefix: "PLAN"})
			if err != nil {
				t.Fatal(err)
			}
			it, err := f.Store.CreateItem(f.Workspace.ID, plan.ID, models.ItemCreate{Title: "Moved plan"})
			if err != nil {
				t.Fatal(err)
			}
			task, err := f.Store.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Tasks", Prefix: "TASK"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Store.MoveItem(it.ID, task.ID, `{}`); err != nil {
				t.Fatal(err)
			}
			base := "/api/v1/workspaces/" + f.Workspace.Slug + "/items/"
			for _, ref := range []string{it.Ref, fmt.Sprintf("TASK-%d", *it.ItemNumber), it.ID} {
				if rr := doRequest(srv, "GET", base+ref, nil); rr.Code != 200 {
					t.Errorf("show %s: %d %s", ref, rr.Code, rr.Body.String())
				}
			}
			rr := doRequest(srv, "POST", base+it.Ref+"/comments", map[string]string{"body": "Old ref comment"})
			if rr.Code != 201 {
				t.Fatalf("comment via moved ref: %d %s", rr.Code, rr.Body.String())
			}
			var c struct {
				ItemID  string `json:"item_id"`
				ItemRef string `json:"item_ref"`
			}
			parseJSON(t, rr, &c)
			if c.ItemID != it.ID || c.ItemRef != fmt.Sprintf("TASK-%d", *it.ItemNumber) {
				t.Errorf("comment response item_id=%q item_ref=%q", c.ItemID, c.ItemRef)
			}
			slugged, err := f.Store.CreateItem(f.Workspace.ID, task.ID, models.ItemCreate{Title: "Ident 999"})
			if err != nil {
				t.Fatal(err)
			}
			if slugged.Slug != "ident-999" {
				t.Fatal(slugged.Slug)
			}
			if rr := doRequest(srv, "GET", base+"IDENT-999", nil); rr.Code != 404 {
				t.Errorf("ref-shaped miss: got %d, want 404", rr.Code)
			}
		})
	}
}

func TestBUG4119_RelationWrongPrefix(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, exists := range []bool{false, true} {
			t.Run(fmt.Sprintf("pg=%t/ident=%t", pg, exists), func(t *testing.T) {
				f := storetest.NewRefPrefixFixture(t, pg, exists)
				srv := New(f.Store)
				t.Cleanup(srv.Stop)
				schema := fmt.Sprintf(`{"fields":[{"key":"target","type":"relation","collection":%q}]}`, f.Item.CollectionSlug)
				coll, err := f.Store.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Sources", Schema: schema})
				if err != nil {
					t.Fatal(err)
				}
				source, err := f.Store.CreateItem(f.Workspace.ID, coll.ID, models.ItemCreate{Title: "Source"})
				if err != nil {
					t.Fatal(err)
				}
				before := f.Snapshot(t)
				rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+f.Workspace.Slug+"/items/"+source.ID, map[string]any{"fields": map[string]any{"target": f.WrongRef}})
				if rr.Code != 400 {
					t.Errorf("relation wrong-prefix: got %d, want 400: %s", rr.Code, rr.Body.String())
				}
				after, err := f.Store.GetItem(source.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.Fields != source.Fields || after.Seq != source.Seq {
					t.Errorf("relation wrote source fields: %s", after.Fields)
				}
				f.AssertUnchanged(t, before)
			})
		}
	}
}

func TestBUG4119_HTTPLowercaseRefSlug(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, route := range []struct {
			name, method, suffix string
			status               int
		}{
			{"update", "PATCH", "", 200}, {"delete", "DELETE", "", 204},
			{"show", "GET", "", 200}, {"progress", "GET", "/progress", 200},
			{"links", "GET", "/links", 200}, {"preflight", "POST", "/copy/preflight", 200},
			{"copy", "POST", "/copy", 201},
		} {
			t.Run(fmt.Sprintf("pg=%t/%s", pg, route.name), func(t *testing.T) {
				f, slugged := storetest.NewRefSlugFixture(t, pg)
				s := f.Store
				srv := New(s)
				t.Cleanup(srv.Stop)
				children, err := s.CreateCollection(f.Workspace.ID, models.CollectionCreate{
					Name: "Work children", Prefix: "CHILD",
					Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"],"default":"open"}]}`,
				})
				if err != nil {
					t.Fatal(err)
				}
				child, err := s.CreateItem(f.Workspace.ID, children.ID, models.ItemCreate{Title: "Slug child"})
				if err != nil {
					t.Fatal(err)
				}
				link, err := s.SetParentLink(f.Workspace.ID, child.ID, slugged.ID, "user")
				if err != nil {
					t.Fatal(err)
				}
				dest, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Copy destination"})
				if err != nil {
					t.Fatal(err)
				}
				coll, err := s.CreateCollection(dest.ID, models.CollectionCreate{Name: "Docs", Schema: `{"fields":[]}`})
				if err != nil {
					t.Fatal(err)
				}
				// Scope the existing mutation snapshot to #2: legitimate writes to the
				// slug owner and destination must not mask writes to the number holder.
				snapshot := func() string {
					t.Helper()
					item, err := s.GetItemIncludeDeleted(f.Item.ID)
					if err != nil {
						t.Fatal(err)
					}
					b, err := json.Marshal(item)
					if err != nil {
						t.Fatal(err)
					}
					result := string(b)
					for _, query := range []string{
						`SELECT COUNT(*) FROM item_versions WHERE item_id = ?`,
						`SELECT COUNT(*) FROM comments WHERE item_id = ?`,
						`SELECT COUNT(*) FROM activities WHERE document_id = ?`,
						`SELECT COUNT(*) FROM event_outbox WHERE subject_id = ?`,
					} {
						var count int
						if err := s.DB().QueryRow(s.D().Rebind(query), f.Item.ID).Scan(&count); err != nil {
							t.Fatal(err)
						}
						result += fmt.Sprintf("/%d", count)
					}
					return result
				}
				before := snapshot()
				var body any
				if route.method == "PATCH" {
					body = map[string]string{"content": "Updated slug owner"}
				}
				if route.method == "POST" {
					body = map[string]string{"target_workspace": dest.Slug, "target_collection": coll.Slug}
				}
				rr := doRequest(srv, route.method, "/api/v1/workspaces/"+f.Workspace.Slug+"/items/release-2"+route.suffix, body)
				if after := snapshot(); after != before {
					t.Errorf("number 2 changed: before=%s after=%s", before, after)
				}
				if rr.Code != route.status {
					t.Fatalf("%s release-2: got %d want %d: %s", route.name, rr.Code, route.status, rr.Body.String())
				}
				switch route.name {
				case "update", "delete":
					got, err := s.GetItemIncludeDeleted(slugged.ID)
					if err != nil {
						t.Fatal(err)
					}
					if route.name == "update" && got.Content != "Updated slug owner" {
						t.Errorf("slug owner not updated: %q", got.Content)
					}
					if route.name == "delete" && got.DeletedAt == nil {
						t.Error("slug owner not deleted")
					}
				case "show":
					var got models.Item
					parseJSON(t, rr, &got)
					if got.ID != slugged.ID {
						t.Errorf("show id=%s want=%s", got.ID, slugged.ID)
					}
				case "progress":
					var got struct{ Total int }
					parseJSON(t, rr, &got)
					if got.Total != 1 {
						t.Errorf("slug owner progress total=%d want=1", got.Total)
					}
				case "links":
					var got []models.ItemLink
					parseJSON(t, rr, &got)
					if len(got) != 1 || got[0].ID != link.ID {
						t.Errorf("slug owner links=%v want=%s", got, link.ID)
					}
				case "preflight":
					var got ItemCopyPreflight
					parseJSON(t, rr, &got)
					if got.Source.Ref != slugged.Ref || got.Source.Slug != slugged.Slug {
						t.Errorf("preflight source=%+v", got.Source)
					}
				case "copy":
					var got ItemCopyResult
					parseJSON(t, rr, &got)
					if got.Source.Ref != slugged.Ref || got.Item == nil || got.Item.Content != slugged.Content || got.Item.Title != slugged.Title {
						t.Errorf("copied wrong item: %+v", got)
					}
				}
			})
		}
	}
}

func TestBUG4119_HTTPLowercaseOldRef(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprintf("pg=%t", pg), func(t *testing.T) {
			f, _ := storetest.NewRefSlugFixture(t, pg)
			plan, err := f.Store.CreateCollection(f.Workspace.ID, models.CollectionCreate{Name: "Plans", Prefix: "PLAN"})
			if err != nil {
				t.Fatal(err)
			}
			it, err := f.Store.CreateItem(f.Workspace.ID, plan.ID, models.ItemCreate{Title: "Moved plan"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Store.MoveItem(it.ID, f.Item.CollectionID, `{}`); err != nil {
				t.Fatal(err)
			}
			srv := New(f.Store)
			t.Cleanup(srv.Stop)
			base := "/api/v1/workspaces/" + f.Workspace.Slug + "/items/"
			for _, ref := range []string{strings.ToLower(it.Ref), "RELEASE-2", "Release-2", "IDENT-10", "ident-10"} {
				if rr := doRequest(srv, "GET", base+ref, nil); rr.Code != 404 {
					t.Errorf("show %s: got %d want 404", ref, rr.Code)
				}
			}
			if rr := doRequest(srv, "GET", base+it.Ref, nil); rr.Code != 200 {
				t.Errorf("uppercase old ref: got %d want 200", rr.Code)
			}
		})
	}
}
