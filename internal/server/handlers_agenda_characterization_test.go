package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The role-aware agenda wire contract for a fixed world. Random schedule IDs
// are masked; every other byte is compared as served.

var agendaScheduleIDRe = regexp.MustCompile(`"schedule_id":"[^"]+"`)

func agendaBytes(t *testing.T, w *agendaWorld, query string) string {
	t.Helper()
	tok, err := w.srv.store.CreateSession(w.owner.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(w.srv, "GET", "/api/v1/workspaces/"+w.ws.Slug+"/agenda?"+query, nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("agenda: %d %s", rr.Code, rr.Body.String())
	}
	return agendaScheduleIDRe.ReplaceAllString(strings.TrimSpace(rr.Body.String()), `"schedule_id":"<id>"`)
}

func TestAgendaCharacterization(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks", "ideas")
	w.srv.agendaClock = func() time.Time { return rtServer("2031-03-01T12:00:00Z") }
	w.item(t, "tasks", "CH sched", `{"status":"open"}`, oneShot())
	w.item(t, "ideas", "CH weekly", `{"status":"open"}`, &models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-03T14:30", Tz: "UTC", Rrule: "FREQ=DAILY;COUNT=40"})
	w.item(t, "tasks", "CH dated", `{"status":"open","due_date":"2031-03-10","end_date":"2031-03-11"}`, nil)
	w.item(t, "tasks", "CH done", `{"status":"done","due_date":"2031-03-10"}`, nil)
	got := agendaBytes(t, w, agendaWindow+"&tz=America/New_York")
	if got != characterizationAgendaWant {
		t.Errorf("agenda bytes changed:\n got: %s\nwant: %s", got, characterizationAgendaWant)
	}

	// Over the entry cap: the dated half is cut and no next_from is promised.
	big := newAgendaWorld(t, "tasks")
	for i := 0; i < agendaMaxEntries+2; i++ {
		big.item(t, "tasks", fmt.Sprintf("CB %04d", i), `{"status":"open","due_date":"2031-03-10"}`, nil)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(agendaBytes(t, big, agendaWindow)), &m); err != nil {
		t.Fatal(err)
	}
	if m["truncated"] != true || m["next_from"] != nil || len(entriesOf(m)) != agendaMaxEntries {
		t.Errorf("cut agenda: truncated=%v next_from=%v entries=%d", m["truncated"], m["next_from"], len(entriesOf(m)))
	}
}

const characterizationAgendaWant = `{"forecasts":[],"forecasts_complete":true,"window_complete":true,"priority_list":[{"type":"schedule","at":"2031-03-03T09:30:00-05:00","day":"2031-03-03","wall":"2031-03-03T14:30:00","state":"outstanding","item_ref":"P1-2","item_title":"CH weekly","collection":"ideas","roles":{"target":"2031-03-03T14:30:00Z","earliest":null,"deadline":null,"deadline_behavior":null,"occupied_until":null,"outcome":"","roles_origin":"legacy_backfill"},"occurrence_key":"2031-03-03T14:30:00Z|2031-03-03T14:30:00","rank":"unranked","reason":"outstanding","roles_origin":"legacy_backfill","audience":"both","schedule_id":"<id>"},{"type":"schedule","at":"2031-03-10T05:00:00-04:00","day":"2031-03-10","wall":"2031-03-10T09:00:00","state":"outstanding","item_ref":"P0-1","item_title":"CH sched","collection":"tasks","roles":{"target":"2031-03-10T09:00:00Z","earliest":null,"deadline":null,"deadline_behavior":null,"occupied_until":null,"outcome":"","roles_origin":"legacy_backfill"},"occurrence_key":"2031-03-10T09:00:00Z|2031-03-10T09:00:00","rank":"unranked","reason":"outstanding","roles_origin":"legacy_backfill","audience":"both","schedule_id":"<id>"}],"priority_list_complete":true,"waiting_on":[],"waiting_on_complete":true,"no_date":[],"no_date_complete":true,"no_date_scope":"personal_only","from":"2031-03-10T00:00:00Z","to":"2031-03-11T00:00:00Z","tz":"America/New_York","entries":[{"type":"date","at":"2031-03-10T00:00:00-04:00","day":"2031-03-10","item_ref":"P0-3","item_title":"CH dated","collection":"tasks","field":"due_date","date":"2031-03-10"},{"type":"schedule","at":"2031-03-10T05:00:00-04:00","day":"2031-03-10","wall":"2031-03-10T09:00:00","floating":false,"state":"outstanding","item_ref":"P0-1","item_title":"CH sched","collection":"tasks","roles":{"target":"2031-03-10T09:00:00Z","earliest":null,"deadline":null,"deadline_behavior":null,"occupied_until":null,"outcome":"","roles_origin":"legacy_backfill"},"occurrence_key":"2031-03-10T09:00:00Z|2031-03-10T09:00:00","roles_origin":"legacy_backfill","audience":"both","schedule_id":"<id>"},{"type":"schedule","at":"2031-03-10T10:30:00-04:00","day":"2031-03-10","wall":"2031-03-10T14:30:00","floating":false,"state":"scheduled","item_ref":"P1-2","item_title":"CH weekly","collection":"ideas","roles":{"target":"2031-03-10T14:30:00Z","earliest":null,"deadline":null,"deadline_behavior":null,"occupied_until":null,"outcome":"","roles_origin":"legacy_backfill"},"occurrence_key":"2031-03-10T14:30:00Z|2031-03-10T14:30:00","roles_origin":"legacy_backfill","audience":"both","schedule_id":"<id>"}]}`
