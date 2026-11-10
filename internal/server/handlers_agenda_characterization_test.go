package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The agenda's wire bytes for a fixed world, captured before the schedule half
// moved into a helper shared with the calendar (TASK-4239). Schedule ids are
// random, so they are masked; everything else is compared as served.

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

const characterizationAgendaWant = `{"from":"2031-03-10T00:00:00Z","to":"2031-03-11T00:00:00Z","tz":"America/New_York","entries":[{"type":"date","at":"2031-03-10T00:00:00-04:00","day":"2031-03-10","item_ref":"P0-3","item_title":"CH dated","collection":"tasks","field":"due_date","date":"2031-03-10"},{"type":"schedule","at":"2031-03-10T05:00:00-04:00","day":"2031-03-10","wall":"2031-03-10T09:00:00","floating":false,"state":"outstanding","item_ref":"P0-1","item_title":"CH sched","collection":"tasks","kind":"check","roles_origin":"legacy_backfill","audience":"both","schedule_id":"<id>"},{"type":"schedule","at":"2031-03-10T10:30:00-04:00","day":"2031-03-10","wall":"2031-03-10T14:30:00","floating":false,"state":"scheduled","item_ref":"P1-2","item_title":"CH weekly","collection":"ideas","kind":"check","roles_origin":"legacy_backfill","audience":"both","schedule_id":"<id>"}]}`
