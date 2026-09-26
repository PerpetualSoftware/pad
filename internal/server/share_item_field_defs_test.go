package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2248 U3 (audit C121). A single-item share carries field defs for the
// fields the item shows, so its chips can be labelled and coloured, and
// carries nothing else about the schema. A link to one item never disclosed its
// collection's option lists, relation targets, defaults or rules, and must not
// start to.
const u3Schema = `{"fields":[
	{"key":"status","label":"Status","type":"select","options":["todo","doing","shipped","dropped"],"terminal_options":["shipped","dropped"],"abandoned_options":["dropped"],"default":"todo","required":true},
	{"key":"owner","label":"Owner","type":"relation","collection":"people"},
	{"key":"secret_codename","label":"Codename","type":"text"},
	{"key":"score","label":"Score","type":"number","computed":true},
	{"key":"effort","label":"Effort","type":"select","options":["s","m","l"]}
]}`

func shareFieldDefsFor(t *testing.T, srv *Server, wsID, collID, ownerID, fields string) (map[string]any, []map[string]any) {
	t.Helper()
	it, err := srv.store.CreateItem(wsID, collID, models.ItemCreate{Title: "Shared", Fields: fields})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	link, err := srv.store.CreateShareLink(wsID, "item", it.ID, "view", ownerID, nil)
	if err != nil {
		t.Fatalf("create share link: %v", err)
	}
	rr := doRequest(srv, "GET", "/api/v1/s/"+link.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve share: %d: %s", rr.Code, rr.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var defs []map[string]any
	if d, ok := raw["field_defs"]; ok {
		b, _ := json.Marshal(d)
		_ = json.Unmarshal(b, &defs)
	}
	return raw, defs
}

func TestItemShareCarriesDefsOnlyForTheFieldsItShows(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("get workspace: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{Email: "u3-owner@test.com", Name: "Owner", Password: "pw-owner"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Features", Schema: u3Schema})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}

	t.Run("a terminal value: its own def, in schema order, and nothing else", func(t *testing.T) {
		raw, defs := shareFieldDefsFor(t, srv, ws.ID, coll.ID, owner.ID, `{"status":"shipped","effort":"m","score":7,"secret_codename":""}`)
		want := []map[string]any{
			{"key": "status", "label": "Status", "type": "select", "terminal_options": []any{"shipped"}},
			{"key": "effort", "label": "Effort", "type": "select"},
		}
		if !reflect.DeepEqual(defs, want) {
			t.Fatalf("field_defs = %#v\nwant %#v", defs, want)
		}
		// The label and type of each shown field ARE schema facts, sent on
		// purpose: that disclosure is the reviewed decision in #1587, and the
		// DeepEqual above pins exactly what it is. What this scan adds is
		// narrower. None of the listed schema facts beyond those appears
		// anywhere in the response: option lists, the other options,
		// abandoned options, defaults, required, the relation target, and the
		// labels of fields this item carries no value for.
		body, _ := json.Marshal(raw)
		for _, leak := range []string{`"options"`, "doing", "dropped", "abandoned_options", `"default"`, `"required"`, "people", "Codename", "Score"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("the item share discloses %s: %s", leak, body)
			}
		}
	})

	t.Run("a non-terminal value carries no terminal list at all", func(t *testing.T) {
		_, defs := shareFieldDefsFor(t, srv, ws.ID, coll.ID, owner.ID, `{"status":"doing"}`)
		want := []map[string]any{{"key": "status", "label": "Status", "type": "select"}}
		if !reflect.DeepEqual(defs, want) {
			t.Fatalf("field_defs = %#v\nwant %#v", defs, want)
		}
	})

	t.Run("an item with no values carries no key, so the page renders as before", func(t *testing.T) {
		raw, _ := shareFieldDefsFor(t, srv, ws.ID, coll.ID, owner.ID, `{}`)
		if _, ok := raw["field_defs"]; ok {
			t.Fatalf("field_defs present on an item with no values: %v", raw["field_defs"])
		}
	})
}
