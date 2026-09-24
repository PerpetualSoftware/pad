package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// BUG-2367 item 4 (lead ruling (a), day 78): a move or copy that would change
// an item between open, done and abandoned is refused unless the caller names
// the destination done field. The preflight asks through needs_value; the
// doors without that route refuse with the field and its values.

const (
	b2367TasksSchema = `{"fields":[{"key":"status","label":"Status","type":"select",
		"options":["open","done","cancelled"],"terminal_options":["done","cancelled"],
		"abandoned_options":["cancelled"],"default":"open"}]}`
	b2367IdeasSchema = `{"fields":[{"key":"status","label":"Status","type":"select",
		"options":["new","implemented","rejected"],"terminal_options":["implemented","rejected"],
		"abandoned_options":["rejected"],"default":"new"}]}`
	b2367ShippedSchema = `{"fields":[{"key":"status","label":"Status","type":"select",
		"options":["todo","done","shelved"],"terminal_options":["done","shelved"],
		"abandoned_options":["shelved"],"default":"todo"}]}`
)

type b2367StateFixture struct {
	*b2367Fixture
	done map[string]any // a `done` item in `src-tasks`
}

func newB2367StateFixture(t *testing.T) *b2367StateFixture {
	t.Helper()
	f := &b2367Fixture{t: t, srv: testServer(t)}
	f.ws = createWSWithCollections(t, f.srv)
	f.collection(f.ws, "Src Tasks", b2367TasksSchema)
	f.collection(f.ws, "Dst Ideas", b2367IdeasSchema)
	f.collection(f.ws, "Dst Shipped", b2367ShippedSchema)
	return &b2367StateFixture{b2367Fixture: f, done: f.item(f.ws, "src-tasks", "Finished", map[string]any{"status": "done"})}
}

func (f *b2367StateFixture) move(target string, overrides map[string]any) (int, map[string]any) {
	body := map[string]any{"target_collection": target}
	if overrides != nil {
		body["field_overrides"] = overrides
	}
	rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/"+f.done["slug"].(string)+"/move", body)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

const b2367StateMessage = `this would change the item from done (status "done") to open (status "new"); set status explicitly to one of: new, implemented, rejected`

func TestBUG2367_CloseState_SingleMove(t *testing.T) {
	t.Run("a done item that would land open is refused, naming the field and its values", func(t *testing.T) {
		f := newB2367StateFixture(t)
		code, body := f.move("dst-ideas", nil)
		if code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d %v", code, body)
		}
		e := body["error"].(map[string]any)
		if e["code"] != closeStateChangeCode || e["message"] != b2367StateMessage {
			t.Fatalf("error = %v", e)
		}
		d := e["details"].(map[string]any)
		if d["field"] != "status" || d["from"] != "done" || d["to"] != "open" {
			t.Fatalf("details = %v", d)
		}
		if got := f.storedFields(f.ws, f.done["slug"].(string))["status"]; got != "done" {
			t.Fatalf("a refused move wrote the item: status %v", got)
		}
	})
	t.Run("an explicit value is the override", func(t *testing.T) {
		f := newB2367StateFixture(t)
		if code, body := f.move("dst-ideas", map[string]any{"status": "new"}); code != http.StatusOK {
			t.Fatalf("want 200, got %d %v", code, body)
		}
	})
	t.Run("two different done values carry without asking", func(t *testing.T) {
		f := newB2367StateFixture(t)
		if code, body := f.move("dst-shipped", nil); code != http.StatusOK {
			t.Fatalf("done → done must carry, got %d %v", code, body)
		}
	})
}

func TestBUG2367_CloseState_BulkMove(t *testing.T) {
	f := newB2367StateFixture(t)
	rr := doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/bulk",
		map[string]any{"ids": []string{f.done["id"].(string)}, "op": "move", "collection": "dst-ideas"})
	var resp struct {
		Updated []any `json:"updated"`
		Failed  []struct {
			Code, Error string
		} `json:"failed"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Updated) != 0 || len(resp.Failed) != 1 || resp.Failed[0].Code != closeStateChangeCode || resp.Failed[0].Error != b2367StateMessage {
		t.Fatalf("bulk: %s", rr.Body.String())
	}
	// `move` with a `status` has supplied the value.
	rr = doRequest(f.srv, "POST", "/api/v1/workspaces/"+f.ws+"/items/bulk",
		map[string]any{"ids": []string{f.done["id"].(string)}, "op": "move", "collection": "dst-ideas", "status": "implemented"})
	parseJSON(t, rr, &resp)
	if len(resp.Failed) != 0 {
		t.Fatalf("a bulk move naming the status must pass: %s", rr.Body.String())
	}
}

func TestBUG2367_CloseState_PreflightAndCopy(t *testing.T) {
	f := newB2367StateFixture(t)
	base := "/api/v1/workspaces/" + f.ws + "/items/" + f.done["slug"].(string)
	body := map[string]any{"target_workspace": f.ws, "target_collection": "dst-ideas"}

	pre := doRequest(f.srv, "POST", base+"/copy/preflight", body)
	var p struct {
		Valid  bool `json:"valid"`
		Fields struct {
			NeedsValue []struct {
				Key, Reason, Message string
				Options              []string
				Required             bool
			} `json:"needs_value"`
		} `json:"fields"`
	}
	parseJSON(t, pre, &p)
	if p.Valid || len(p.Fields.NeedsValue) != 1 {
		t.Fatalf("preflight: %s", pre.Body.String())
	}
	row := p.Fields.NeedsValue[0]
	if row.Key != "status" || row.Reason != "state_change" || !row.Required || row.Message != b2367StateMessage || len(row.Options) != 3 {
		t.Fatalf("needs_value row = %+v", row)
	}

	cp := doRequest(f.srv, "POST", base+"/copy", body)
	if cp.Code != http.StatusBadRequest || !jsonHasCode(t, cp.Body, "validation_error") {
		t.Fatalf("copy without the value: %d %s", cp.Code, cp.Body.String())
	}

	body["field_overrides"] = map[string]any{"status": "implemented"}
	pre = doRequest(f.srv, "POST", base+"/copy/preflight", body)
	parseJSON(t, pre, &p)
	if !p.Valid {
		t.Fatalf("preflight with the value should be valid: %s", pre.Body.String())
	}
	if cp = doRequest(f.srv, "POST", base+"/copy", body); cp.Code != http.StatusCreated {
		t.Fatalf("copy with the value: %d %s", cp.Code, cp.Body.String())
	}
}
