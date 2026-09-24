package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2367: MigrateFields matched fields by key and type only, so every
// migrate door — single move, bulk move, the copy and its preflight — carried
// a value into a COMPUTED destination field, and carried a value that
// collides on a unique_scope field (single move answered 500 on the indexed
// invocation_slug, bulk move leaked the SQL error, and an unindexed unique
// field stored the duplicate).
//
// The rule every door now shares (lead rulings, day 78): a computed target is
// never carried into (`target_computed`); a CARRIED unique collision is dropped
// and reported (`not_unique`), naming the value and — when the caller may see
// it — the item holding it; a SUPPLIED override or an injected default that
// collides is refused 409.

const (
	b2367Src = `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"invocation_slug","label":"Slug","type":"text"},
		{"key":"progress","label":"Progress","type":"number"}
	]}`
	b2367Dst = `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"invocation_slug","label":"Slug","type":"text","unique_scope":"workspace_collection"},
		{"key":"progress","label":"Progress","type":"number","computed":true}
	]}`
	b2367DstRequired = `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"invocation_slug","label":"Slug","type":"text","unique_scope":"workspace_collection","required":true}
	]}`
	b2367DstDefault = `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"code","label":"Code","type":"text","unique_scope":"workspace_collection","default":"dup"}
	]}`
)

type b2367Fixture struct {
	t      *testing.T
	srv    *Server
	ws     string
	source map[string]any // in `src`, invocation_slug "day", progress 40
	holder map[string]any // in `dst`, invocation_slug "day"
}

func (f *b2367Fixture) collection(ws, name, schema string) string {
	f.t.Helper()
	rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]any{"name": name, "schema": schema})
	if rr.Code != http.StatusCreated {
		f.t.Fatalf("create collection %s: %d %s", name, rr.Code, rr.Body.String())
	}
	var c map[string]any
	parseJSON(f.t, rr, &c)
	return c["slug"].(string)
}

func (f *b2367Fixture) item(ws, coll, title string, fields map[string]any) map[string]any {
	f.t.Helper()
	rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll+"/items", map[string]any{"title": title, "fields": fields})
	if rr.Code != http.StatusCreated {
		f.t.Fatalf("create item %s: %d %s", title, rr.Code, rr.Body.String())
	}
	var it map[string]any
	parseJSON(f.t, rr, &it)
	return it
}

func (f *b2367Fixture) storedFields(ws, slug string) map[string]any {
	f.t.Helper()
	rr := doRequest(f.srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+slug, nil)
	var it struct {
		Fields string `json:"fields"`
	}
	parseJSON(f.t, rr, &it)
	var m map[string]any
	if err := json.Unmarshal([]byte(it.Fields), &m); err != nil {
		f.t.Fatalf("stored fields: %v (%q)", err, it.Fields)
	}
	return m
}

func newB2367Fixture(t *testing.T) *b2367Fixture {
	t.Helper()
	f := &b2367Fixture{t: t, srv: testServer(t)}
	f.ws = createWSWithCollections(t, f.srv)
	f.collection(f.ws, "Src", b2367Src)
	f.collection(f.ws, "Dst", b2367Dst)
	f.source = f.item(f.ws, "src", "Source", map[string]any{"status": "open", "invocation_slug": "day", "progress": 40})
	f.holder = f.item(f.ws, "dst", "Holder", map[string]any{"status": "open", "invocation_slug": "day"})
	return f
}

func (f *b2367Fixture) wantMessage() string {
	return fmt.Sprintf("invocation_slug %q is taken by %s", "day", f.holder["ref"])
}

type b2367NotUnique struct {
	Key, Value, Holder, Message string
}

func TestBUG2367_SingleMove(t *testing.T) {
	t.Run("a carried collision is dropped and named; the computed field is not carried", func(t *testing.T) {
		f := newB2367Fixture(t)
		rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/"+f.source["slug"].(string)+"/move",
			map[string]any{"target_collection": "dst"})
		if rr.Code != http.StatusOK {
			t.Fatalf("move: %d %s", rr.Code, rr.Body.String())
		}
		var moved struct {
			Warnings struct {
				NotUnique []b2367NotUnique `json:"not_unique"`
			} `json:"warnings"`
		}
		parseJSON(t, rr, &moved)
		want := b2367NotUnique{"invocation_slug", "day", f.holder["ref"].(string), f.wantMessage()}
		if len(moved.Warnings.NotUnique) != 1 || moved.Warnings.NotUnique[0] != want {
			t.Fatalf("warnings.not_unique = %+v, want [%+v]", moved.Warnings.NotUnique, want)
		}
		got := f.storedFields(f.ws, f.source["slug"].(string))
		if _, ok := got["invocation_slug"]; ok {
			t.Errorf("the colliding slug was stored: %v", got)
		}
		if _, ok := got["progress"]; ok {
			t.Errorf("a literal was carried into a computed field: %v", got)
		}
		if f.storedFields(f.ws, f.holder["slug"].(string))["invocation_slug"] != "day" {
			t.Error("the holder lost its value")
		}

		// The activity row is read by everyone who can see the moved item, so
		// it carries the holder-free sentence even though the ACTOR may see
		// the holder.
		act := doRequest(f.srv, "GET", "/api/v1/workspaces/"+f.ws+"/items/"+f.source["slug"].(string)+"/activity", nil)
		var activities []models.Activity
		parseJSON(t, act, &activities)
		var meta map[string]any
		for _, a := range activities {
			if a.Action == "moved" {
				_ = json.Unmarshal([]byte(a.Metadata), &meta)
			}
		}
		wantAudit := `invocation_slug "day" is already used by another item in Dst`
		if meta["not_unique"] != wantAudit {
			t.Fatalf("moved activity not_unique = %v, want %q (metadata %v)", meta["not_unique"], wantAudit, meta)
		}
	})

	t.Run("an override that collides is refused 409", func(t *testing.T) {
		f := newB2367Fixture(t)
		rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/"+f.source["slug"].(string)+"/move",
			map[string]any{"target_collection": "dst", "field_overrides": map[string]any{"invocation_slug": "day"}})
		if rr.Code != http.StatusConflict {
			t.Fatalf("want 409, got %d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("an override that does not collide replaces the carried value", func(t *testing.T) {
		f := newB2367Fixture(t)
		rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/"+f.source["slug"].(string)+"/move",
			map[string]any{"target_collection": "dst", "field_overrides": map[string]any{"invocation_slug": "night"}})
		if rr.Code != http.StatusOK {
			t.Fatalf("move: %d %s", rr.Code, rr.Body.String())
		}
		var moved map[string]any
		parseJSON(t, rr, &moved)
		if moved["warnings"] != nil {
			t.Errorf("no drop happened, so no warning: %v", moved["warnings"])
		}
		if got := f.storedFields(f.ws, f.source["slug"].(string))["invocation_slug"]; got != "night" {
			t.Errorf("stored %v, want night", got)
		}
	})

	t.Run("a REQUIRED unique field left empty by the drop is refused as required", func(t *testing.T) {
		f := newB2367Fixture(t)
		f.collection(f.ws, "Dst Req", b2367DstRequired)
		f.item(f.ws, "dst-req", "Req Holder", map[string]any{"status": "open", "invocation_slug": "day"})
		rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/"+f.source["slug"].(string)+"/move",
			map[string]any{"target_collection": "dst-req"})
		if rr.Code != http.StatusBadRequest || !jsonHasCode(t, rr.Body, "missing_required_fields") {
			t.Fatalf("want 400 missing_required_fields, got %d %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("a DEFAULT that collides is refused 409", func(t *testing.T) {
		f := newB2367Fixture(t)
		f.collection(f.ws, "Dst Def", b2367DstDefault)
		f.item(f.ws, "dst-def", "Def Holder", map[string]any{"status": "open", "code": "dup"})
		rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/"+f.source["slug"].(string)+"/move",
			map[string]any{"target_collection": "dst-def"})
		if rr.Code != http.StatusConflict {
			t.Fatalf("want 409, got %d %s", rr.Code, rr.Body.String())
		}
	})
}

func TestBUG2367_BulkMove(t *testing.T) {
	f := newB2367Fixture(t)
	rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/bulk",
		map[string]any{"ids": []string{f.source["id"].(string)}, "op": "move", "collection": "dst"})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Updated []struct {
			NotUnique []b2367NotUnique `json:"not_unique"`
		} `json:"updated"`
		Failed []any `json:"failed"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Failed) != 0 || len(resp.Updated) != 1 {
		t.Fatalf("want one updated row, got %s", rr.Body.String())
	}
	want := b2367NotUnique{"invocation_slug", "day", f.holder["ref"].(string), f.wantMessage()}
	if len(resp.Updated[0].NotUnique) != 1 || resp.Updated[0].NotUnique[0] != want {
		t.Fatalf("updated[0].not_unique = %+v, want [%+v]", resp.Updated[0].NotUnique, want)
	}
	got := f.storedFields(f.ws, f.source["slug"].(string))
	if _, ok := got["invocation_slug"]; ok {
		t.Errorf("the colliding slug was stored: %v", got)
	}
	if _, ok := got["progress"]; ok {
		t.Errorf("a literal was carried into a computed field: %v", got)
	}
}

// The preflight and the copy must drop the same keys with the same words
// (DR-6), same workspace and across workspaces.
func TestBUG2367_PreflightAndCopyAgree(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross_workspace=%v", cross), func(t *testing.T) {
			f := newB2367Fixture(t)
			dstWS := f.ws
			holder := f.holder
			if cross {
				dstWS = createWSWithCollections(t, f.srv)
				f.collection(dstWS, "Dst", b2367Dst)
				holder = f.item(dstWS, "dst", "Far Holder", map[string]any{"status": "open", "invocation_slug": "day"})
			}
			wantMsg := fmt.Sprintf("invocation_slug %q is taken by %s", "day", holder["ref"])
			body := map[string]any{"target_workspace": dstWS, "target_collection": "dst"}
			base := "/api/v1/workspaces/" + f.ws + "/items/" + f.source["slug"].(string)

			pre := doRequest(f.srv, "POST", base+"/copy/preflight", body)
			if pre.Code != http.StatusOK {
				t.Fatalf("preflight: %d %s", pre.Code, pre.Body.String())
			}
			var p struct {
				Fields struct {
					Dropped []struct{ Key, Reason, Detail string } `json:"dropped"`
				} `json:"fields"`
			}
			parseJSON(t, pre, &p)
			reasons := map[string]string{}
			for _, d := range p.Fields.Dropped {
				reasons[d.Key] = d.Reason
				if d.Key == "invocation_slug" && d.Detail != wantMsg {
					t.Errorf("preflight detail = %q, want %q", d.Detail, wantMsg)
				}
			}
			if reasons["invocation_slug"] != "not_unique" || reasons["progress"] != "target_computed" {
				t.Fatalf("preflight dropped = %+v", p.Fields.Dropped)
			}

			cp := doRequest(f.srv, "POST", base+"/copy", body)
			if cp.Code != http.StatusCreated {
				t.Fatalf("copy: %d %s", cp.Code, cp.Body.String())
			}
			var c struct {
				Warnings struct {
					DroppedFields []string         `json:"dropped_fields"`
					NotUnique     []b2367NotUnique `json:"not_unique"`
				} `json:"warnings"`
				Destination struct{ Slug string } `json:"destination"`
			}
			parseJSON(t, cp, &c)
			if len(c.Warnings.NotUnique) != 1 || c.Warnings.NotUnique[0].Message != wantMsg {
				t.Fatalf("copy warnings.not_unique = %+v, want message %q", c.Warnings.NotUnique, wantMsg)
			}
			dropped := map[string]bool{}
			for _, k := range c.Warnings.DroppedFields {
				dropped[k] = true
			}
			if !dropped["invocation_slug"] || !dropped["progress"] {
				t.Errorf("copy dropped_fields = %v, the preflight dropped invocation_slug and progress", c.Warnings.DroppedFields)
			}
			got := f.storedFields(dstWS, c.Destination.Slug)
			if _, ok := got["invocation_slug"]; ok {
				t.Errorf("the copy stored the colliding slug: %v", got)
			}

			// An override that collides: both refuse, with the same status.
			body["field_overrides"] = map[string]any{"invocation_slug": "day"}
			pre = doRequest(f.srv, "POST", base+"/copy/preflight", body)
			cp = doRequest(f.srv, "POST", base+"/copy", body)
			if pre.Code != http.StatusConflict || cp.Code != http.StatusConflict {
				t.Fatalf("colliding override: preflight %d %s / copy %d %s", pre.Code, pre.Body.String(), cp.Code, cp.Body.String())
			}
			if pre.Body.String() != cp.Body.String() {
				t.Errorf("the two refusals differ:\n preflight %s\n copy      %s", pre.Body.String(), cp.Body.String())
			}
		})
	}
}

func jsonHasCode(t *testing.T, rr interface{ Bytes() []byte }, code string) bool {
	t.Helper()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(rr.Bytes(), &e)
	return e.Error.Code == code
}
