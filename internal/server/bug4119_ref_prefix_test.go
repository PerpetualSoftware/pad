package server

import (
	"fmt"
	"net/http"
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
			if rr := doRequest(srv, "GET", base+slugged.Slug, nil); rr.Code != 404 {
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
