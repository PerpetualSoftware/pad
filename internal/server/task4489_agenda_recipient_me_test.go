package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// recipient=me resolves to the caller: the caller's own undated work is
// listed, and the groups match an explicit recipient=<caller id> exactly.
func TestAgendaRecipientMeIsTheCaller(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	undated, err := w.srv.store.CreateItem(w.ws.ID, w.colls["tasks"].ID, models.ItemCreate{Title: "Unscheduled", Fields: `{"status":"open"}`, AssignedUserID: &w.owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	w.sched(t, w.owner, "Scheduled", "2031-03-10T09:00", "")

	me := w.ok(t, w.owner, agendaWindow+"&recipient=me")
	rows, _ := me["no_date"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["item_ref"] != undated.Ref {
		t.Fatalf("recipient=me no_date = %v", me["no_date"])
	}
	if _, has := me["no_date_scope"]; has {
		t.Fatalf("recipient=me carries no_date_scope: %v", me["no_date_scope"])
	}
	byID := w.ok(t, w.owner, agendaWindow+"&recipient="+w.owner.ID)
	for _, k := range []string{"priority_list", "priority_list_complete", "waiting_on", "waiting_on_complete", "no_date", "no_date_complete", "no_date_scope"} {
		a, _ := json.Marshal(me[k])
		b, _ := json.Marshal(byID[k])
		if string(a) != string(b) {
			t.Fatalf("%s: recipient=me %s; recipient=<id> %s", k, a, b)
		}
	}
	if p, _ := me["priority_list"].([]any); len(p) == 0 {
		t.Fatalf("recipient=me priority_list is empty: %v", me)
	}
}

// With no caller user (an open instance with no users), recipient=me is the
// workspace scope: no_date is not listed and carries the personal_only marker.
func TestAgendaRecipientMeWithoutCallerUser(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agenda?"+agendaWindow+"&recipient=me", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("agenda: %d %s", rr.Code, rr.Body.String())
	}
	m := map[string]any{}
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if rows, ok := m["no_date"].([]any); !ok || len(rows) != 0 || m["no_date_scope"] != "personal_only" {
		t.Fatalf("no-user recipient=me: no_date=%v no_date_scope=%v", m["no_date"], m["no_date_scope"])
	}
}
