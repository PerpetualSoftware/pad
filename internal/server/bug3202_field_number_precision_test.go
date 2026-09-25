package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3202. Every door that writes an item's `fields` decoded the blob into
// map[string]any, so each JSON number became a float64 and the re-encode
// rounded an integer above 2^53 (…993 stored as …992). It rounded values the
// write never named: a fields_patch to `status` rewrote every large integer in
// the row.
//
// One row per write door, each asserted on the RAW stored blob. The item is
// created at the store, which writes the blob verbatim, so the door under
// test is the only thing that could have re-encoded it; the first row is that
// control. `n` is a declared number field, `u` an undeclared key, so both the
// validated and the carried-through paths are covered.
func TestFieldWriteDoorsKeepIntegersAbove2Pow53(t *testing.T) {
	const bigN = "9007199254740993" // declared number field
	const bigU = "9007199254740995" // undeclared key, carried
	const bigS = "9007199254740997" // a SUPPLIED n, distinct from the stored one so a no-op write cannot pass

	type door struct {
		name string
		run  func(t *testing.T, srv *Server, ws string, it *models.Item)
		// dropsU: the door drops undeclared keys by design (a move carries
		// only what the destination declares), so only `n` is asserted.
		dropsU bool
		// landed is a fragment of the stored blob that proves the door's
		// OWN change was written. Without it a door that wrote nothing
		// would pass, because an untouched blob keeps its numbers.
		landed string
		// seed is extra members for the stored blob, for a door that only
		// runs on a particular stored state.
		seed string
	}
	send := func(t *testing.T, srv *Server, method, path, body string) {
		t.Helper()
		rr := rawJSONRequest(srv, method, path, body)
		if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, rr.Code, rr.Body.String())
		}
	}
	item := func(ws string, it *models.Item) string { return "/api/v1/workspaces/" + ws + "/items/" + it.Slug }
	doors := []door{
		{"control: no door (store create only)", func(t *testing.T, srv *Server, ws string, it *models.Item) {}, false, "", ""},
		{"fields_patch other key", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "PATCH", item(ws, it), `{"fields_patch":{"status":"done"}}`)
		}, false, `"status":"done"`, ""},
		{"fields_patch supplies n", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "PATCH", item(ws, it), `{"fields_patch":{"n":`+bigS+`}}`)
		}, false, `"n":` + bigS, ""},
		{"full fields echo", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "PATCH", item(ws, it), `{"fields":{"status":"done","n":`+bigN+`,"u":`+bigU+`}}`)
		}, false, `"status":"done"`, ""},
		{"append note", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "PATCH", item(ws, it), `{"append_implementation_note":{"summary":"s"}}`)
		}, false, `"implementation_notes":[`, ""},
		{"title only", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "PATCH", item(ws, it), `{"title":"renamed"}`)
		}, false, "", ""},
		{"move", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "POST", item(ws, it)+"/move", `{"target_collection":"nums2"}`)
		}, true, "", ""},
		{"bulk move status", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", `{"op":"move","ids":["`+it.ID+`"],"status":"done"}`)
		}, false, `"status":"done"`, ""},
		{"move override supplies n", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "POST", item(ws, it)+"/move", `{"target_collection":"nums2","field_overrides":{"n":`+bigS+`}}`)
		}, true, "", ""},
		{"fields_patch n as string (CLI --field)", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "PATCH", item(ws, it), `{"fields_patch":{"n":"`+bigS+`"}}`)
		}, false, `"n":` + bigS, ""},
		{"bulk move to another collection", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", `{"op":"move","ids":["`+it.ID+`"],"collection":"nums2","status":"done"}`)
		}, true, `"status":"done"`, ""},
		// A json field written as TEXT (the CLI's --field j='{...}') is parsed
		// by the coercion step, which must keep the literal inside it.
		{"fields_patch json field as string", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "PATCH", item(ws, it), `{"fields_patch":{"j":"{\"x\":`+bigS+`}"}}`)
		}, false, `"j":{"x":` + bigS + `}`, ""},
		// A legacy blank relation the write only CARRIES is normalised away by
		// the store under its lock (BUG-3028), a second re-encode of the blob.
		{"fields_patch over a carried blank relation", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "PATCH", item(ws, it), `{"fields_patch":{"status":"done"}}`)
			if raw := mustGetItemFields(t, srv, it.ID); strings.Contains(raw, `"r":`) {
				t.Fatalf("premise: the carried blank relation was not normalised away, so the second re-encode did not run: %s", raw)
			}
		}, false, `"status":"done"`, `,"r":""`},
		{"bulk set-priority", func(t *testing.T, srv *Server, ws string, it *models.Item) {
			send(t, srv, "POST", "/api/v1/workspaces/"+ws+"/items/bulk", `{"op":"set-priority","ids":["`+it.ID+`"],"priority":"high"}`)
		}, false, `"priority":"high"`, ""},
	}
	for _, d := range doors {
		t.Run(d.name, func(t *testing.T) {
			srv := testServer(t)
			ws := createWSWithCollections(t, srv)
			wsm, _ := srv.store.GetWorkspaceBySlug(ws)
			for _, name := range []string{"Nums", "Nums2"} {
				schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]},` +
					`{"key":"priority","type":"select","options":["low","high"]},{"key":"n","type":"number"},{"key":"j","type":"json"},` +
					`{"key":"r","type":"relation","collection":"nums"}]}`
				rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]any{"name": name, "schema": schema})
				if rr.Code != http.StatusCreated {
					t.Fatalf("coll %d %s", rr.Code, rr.Body.String())
				}
			}
			coll, _ := srv.store.GetCollectionBySlug(wsm.ID, "nums")
			it, err := srv.store.CreateItem(wsm.ID, coll.ID, models.ItemCreate{Title: "x",
				Fields: `{"status":"open","n":` + bigN + `,"u":` + bigU + d.seed + `}`})
			if err != nil {
				t.Fatal(err)
			}
			d.run(t, srv, ws, it)
			raw := mustGetItemFields(t, srv, it.ID)
			if d.landed != "" && !strings.Contains(raw, d.landed) {
				t.Fatalf("premise: the door's own change (%s) is not in the stored blob: %s", d.landed, raw)
			}
			if strings.HasPrefix(d.name, "move") {
				moved, err := srv.store.GetItem(it.ID)
				if err != nil || moved.CollectionID == it.CollectionID {
					t.Fatalf("premise: the item did not move (err=%v)", err)
				}
			}
			if strings.HasPrefix(d.name, "title") {
				if got, _ := srv.store.GetItem(it.ID); got == nil || got.Title != "renamed" {
					t.Fatal("premise: the title write did not land")
				}
			}
			wantN := bigN
			if strings.Contains(d.name, "supplies n") || strings.Contains(d.name, "n as string") {
				wantN = bigS
			}
			if !strings.Contains(raw, `"n":`+wantN) {
				t.Errorf("declared number n was not stored as %s: %s", wantN, raw)
			}
			if !d.dropsU && !strings.Contains(raw, `"u":`+bigU) {
				t.Errorf("undeclared key u was not stored as %s: %s", bigU, raw)
			}
		})
	}
	t.Run("cross-workspace copy (carried and override)", func(t *testing.T) {
		srv := testServer(t)
		schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]},{"key":"n","type":"number"},{"key":"m","type":"number"}]}`
		var wss []string
		for i := 0; i < 2; i++ {
			rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "W" + string(rune('A'+i))})
			var w models.Workspace
			parseJSON(t, rr, &w)
			wss = append(wss, w.Slug)
			rr = doRequest(srv, "POST", "/api/v1/workspaces/"+w.Slug+"/collections", map[string]any{"name": "Nums", "schema": schema})
			if rr.Code != http.StatusCreated {
				t.Fatalf("coll %d %s", rr.Code, rr.Body.String())
			}
		}
		wsm, _ := srv.store.GetWorkspaceBySlug(wss[0])
		coll, _ := srv.store.GetCollectionBySlug(wsm.ID, "nums")
		it, _ := srv.store.CreateItem(wsm.ID, coll.ID, models.ItemCreate{Title: "x", Fields: `{"status":"open","n":` + bigN + `}`})
		// The preflight previews what the copy will write, so it reports the
		// carried value with the same digits the copy stores.
		pre := rawJSONRequest(srv, "POST", "/api/v1/workspaces/"+wss[0]+"/items/"+it.Slug+"/copy/preflight",
			`{"target_workspace":"`+wss[1]+`","target_collection":"nums","field_overrides":{"m":`+bigU+`}}`)
		if pre.Code != http.StatusOK {
			t.Fatalf("preflight: %d %s", pre.Code, pre.Body.String())
		}
		if !strings.Contains(pre.Body.String(), bigN) {
			t.Errorf("preflight does not report the carried n as %s: %s", bigN, pre.Body.String())
		}
		rr := rawJSONRequest(srv, "POST", "/api/v1/workspaces/"+wss[0]+"/items/"+it.Slug+"/copy",
			`{"target_workspace":"`+wss[1]+`","target_collection":"nums","field_overrides":{"m":`+bigU+`}}`)
		if rr.Code != http.StatusCreated {
			t.Fatalf("copy: %d %s", rr.Code, rr.Body.String())
		}
		var out struct {
			Item models.Item `json:"item"`
		}
		parseJSON(t, rr, &out)
		raw := mustGetItemFields(t, srv, out.Item.ID)
		// n is carried from the source; m is a supplied override.
		if !strings.Contains(raw, `"n":`+bigN) || !strings.Contains(raw, `"m":`+bigU) {
			t.Errorf("copy did not keep both integers (n=%s carried, m=%s supplied): %s", bigN, bigU, raw)
		}
	})
	t.Run("create supplies a value", func(t *testing.T) {
		srv := testServer(t)
		ws := createWSWithCollections(t, srv)
		it := createTaskWithFields(t, srv, ws, "c", `{"status":"open","u":`+bigU+`}`)
		raw := mustGetItemFields(t, srv, it.ID)
		if !strings.Contains(raw, `"u":`+bigU) {
			t.Errorf("create did not store u as %s: %s", bigU, raw)
		}
	})
}
