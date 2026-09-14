package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3079 at the DOOR. `internal/items` pins the rule; this pins that a real
// create runs it and that the drop reaches the caller, which a unit test of the
// validator cannot say (CONVE-19).

func bug3079Collection(t *testing.T, srv *Server, ws, name, schema string) models.Collection {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections",
		map[string]interface{}{"name": name, "schema": schema})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection %q: %d %s", name, rr.Code, rr.Body.String())
	}
	var c models.Collection
	parseJSON(t, rr, &c)
	return c
}

type bug3079Created struct {
	Fields   json.RawMessage `json:"fields"`
	Warnings struct {
		DroppedFields []string `json:"dropped_fields"`
	} `json:"warnings"`
}

func TestBUG3079_CreateDoorDropsAnInvalidDefault(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	// `status` retyped to multi_select; the scalar default survived.
	coll := bug3079Collection(t, srv, ws, "Stale",
		`{"fields":[{"key":"status","type":"multi_select","options":["open","done"],"default":"open"}]}`)

	// PREMISE, asserted rather than assumed: the same bytes SUPPLIED are still
	// refused. Without this the test below passes on a build that accepts
	// everything, and the bug was precisely the two doors disagreeing.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll.Slug+"/items",
		map[string]interface{}{"title": "Supplied", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("a supplied scalar must still be refused: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll.Slug+"/items",
		map[string]interface{}{"title": "Omitted", "fields": `{}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("the create must still land: %d %s", rr.Code, rr.Body.String())
	}
	var got bug3079Created
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}

	// The assertion that fails on an unfixed build: it stored {"status":"open"}.
	var stored map[string]any
	if err := json.Unmarshal(got.Fields, &stored); err != nil {
		// `fields` serialises as a STRING on this shape; fall back to that.
		var asString string
		if json.Unmarshal(got.Fields, &asString) != nil {
			t.Fatalf("fields is neither object nor string: %s", got.Fields)
		}
		if err := json.Unmarshal([]byte(asString), &stored); err != nil {
			t.Fatalf("fields string is not JSON: %q", asString)
		}
	}
	if v, ok := stored["status"]; ok {
		t.Errorf("the invalid default was stored: status=%#v", v)
	}

	// Silent is the other half of the defect: a schema author cannot fix what
	// nothing reports.
	found := false
	for _, k := range got.Warnings.DroppedFields {
		if k == "status" {
			found = true
		}
	}
	if !found {
		t.Errorf("the drop was not reported: warnings.dropped_fields = %v", got.Warnings.DroppedFields)
	}
}

func TestBUG3079_CreateDoorStillAppliesAGoodDefault(t *testing.T) {
	// Control at the door, for the reason the unit control exists: every
	// assertion above is satisfied by a build that drops every default.
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	coll := bug3079Collection(t, srv, ws, "Healthy",
		`{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll.Slug+"/items",
		map[string]interface{}{"title": "Fine", "fields": `{}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var got bug3079Created
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Warnings.DroppedFields) != 0 {
		t.Fatalf("a healthy default reports nothing: %v", got.Warnings.DroppedFields)
	}
	var asString string
	if json.Unmarshal(got.Fields, &asString) == nil {
		if asString != `{"status":"open"}` {
			t.Errorf("default not applied: %s", asString)
		}
		return
	}
	var stored map[string]any
	if err := json.Unmarshal(got.Fields, &stored); err != nil {
		t.Fatalf("fields: %s", got.Fields)
	}
	if stored["status"] != "open" {
		t.Errorf("default not applied: %#v", stored)
	}
}

func TestBUG3079_RequiredFieldWithABadDefaultRefusesAndSaysWhy(t *testing.T) {
	// The ruled edge, at the door: dropping leaves a required field absent, so
	// the create is refused — and the message names the DEFAULT, because a bare
	// "field is required" points the reader at a request that never mentioned it.
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	coll := bug3079Collection(t, srv, ws, "RequiredStale",
		`{"fields":[{"key":"status","type":"multi_select","options":["open"],"default":"open","required":true}]}`)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll.Slug+"/items",
		map[string]interface{}{"title": "Blocked", "fields": `{}`})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); !strings.Contains(body, "schema default") {
		t.Errorf("the refusal does not name the default as the cause: %s", body)
	}
}
