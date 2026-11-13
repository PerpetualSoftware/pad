package server

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMeAgendaEventsDoNotCutDatedWindow(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	start := meAgendaNow.Add(-10 * time.Hour)
	for i := 0; i < 600; i++ {
		it := w.item(t, "tasks", fmt.Sprintf("Event %03d", i), `{"status":"done"}`, nil)
		at := start.Add(time.Duration(i) * time.Second)
		meAgendaExec(t, w, `UPDATE items SET created_at=? WHERE id=?`, at.Add(-time.Hour).Format(time.RFC3339), it.ID)
		meAgendaTransition(t, w, it, at.Format(time.RFC3339), "done")
	}
	for i := 0; i < 3; i++ {
		w.item(t, "tasks", fmt.Sprintf("Scheduled %d", i), `{}`, oneShot())
	}
	m := meAgendaOK(t, w, w.owner, agendaWindow)
	if m["window_complete"] != true || m["truncated"] != nil || m["next_from"] != nil || m["events_truncated"] != true {
		t.Fatalf("event cap cut the dated window: complete=%v truncated=%v next=%v events_truncated=%v", m["window_complete"], m["truncated"], m["next_from"], m["events_truncated"])
	}
	kinds := map[string]int{}
	var lastEvent string
	for _, e := range entriesOf(m) {
		kinds[e["type"].(string)]++
		if e["type"] == "completed" {
			lastEvent = e["at"].(string)
		}
	}
	if kinds["schedule"] != 3 || kinds["created"] != 0 || kinds["completed"] != 500 || len(entriesOf(m)) != 503 {
		t.Fatalf("entries by kind = %v", kinds)
	}
	if want := start.Add(499 * time.Second).Format(time.RFC3339); lastEvent != want {
		t.Fatalf("last event = %s, want earliest 500 ending at %s", lastEvent, want)
	}
}

func TestMeAgendaInclude(t *testing.T) {
	w := newAgendaWorld(t, "tasks")
	meAgendaFacts(t, w, "tasks", "Included")
	for _, tc := range []struct{ include, want string }{
		{"created,completed", "completed:Included,created:Included,date:Included,schedule:Included"},
		{"", "date:Included,schedule:Included"},
		{" completed , created , completed ", "completed:Included,created:Included,date:Included,schedule:Included"},
		{"created", "created:Included,date:Included,schedule:Included"},
	} {
		t.Run(tc.include, func(t *testing.T) {
			m := meAgendaOK(t, w, w.owner, agendaWindow+"&include="+url.QueryEscape(tc.include))
			if got := seen(m); got != tc.want {
				t.Fatalf("entries = %s, want %s", got, tc.want)
			}
			if m["events_truncated"] != nil {
				t.Fatalf("uncapped events reported truncated: %v", m)
			}
		})
	}
	for _, value := range []string{"bogus", "created,bogus", "created,", ",completed"} {
		t.Run("invalid "+value, func(t *testing.T) {
			// A group lookup would return 404: include must be validated first.
			rr := meAgendaGet(t, w, w.owner, agendaWindow+"&group=missing&include="+url.QueryEscape(value))
			if rr.Code != 400 || !strings.Contains(rr.Body.String(), `"field":"include"`) || !strings.Contains(rr.Body.String(), `"code":"format"`) || !strings.Contains(rr.Body.String(), "`include` must be a comma-separated list of created and completed") {
				t.Fatalf("invalid include = %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestMeAgendaExcludedEventsAreNotRead(t *testing.T) {
	for _, kind := range []string{"created", "completed"} {
		t.Run(kind, func(t *testing.T) {
			w := newAgendaWorld(t, "tasks")
			it := w.item(t, "tasks", "Malformed event", `{"status":"done"}`, nil)
			if kind == "created" {
				meAgendaExec(t, w, `UPDATE items SET created_at='2031-03-10T01:00:00Z' WHERE id=?`, it.ID)
			} else {
				meAgendaTransition(t, w, it, "2031-03-10T02:00:00Z", "done")
			}
			// Only the event reader scans this value into an integer. A successful
			// excluded response proves the SQL read was skipped, rather than filtered.
			meAgendaExec(t, w, `UPDATE items SET item_number='bad' WHERE id=?`, it.ID)
			if rr := meAgendaGet(t, w, w.owner, agendaWindow+"&include="+kind); rr.Code != 500 {
				t.Fatalf("included malformed %s read = %d %s", kind, rr.Code, rr.Body.String())
			}
			queries := []string{agendaWindow + "&include="}
			if kind == "created" {
				queries = append(queries, agendaWindow, agendaWindow+"&include=completed")
			} else {
				queries = append(queries, agendaWindow+"&include=created")
			}
			for _, query := range queries {
				m := meAgendaOK(t, w, w.owner, query)
				if len(entriesOf(m)) != 0 {
					t.Fatalf("excluded events returned: %v", m)
				}
			}
		})
	}
}

func TestAgendaEventCapPreservesDatedResume(t *testing.T) {
	for _, kind := range []string{"schedule", "date"} {
		t.Run(kind, func(t *testing.T) {
			run := &agendaRead{from: meAgendaNow, to: meAgendaNow.Add(time.Hour)}
			for i := 500; i >= 0; i-- {
				at := meAgendaNow.Add(time.Duration(i) * time.Second)
				eventKind := "created"
				if i%2 == 1 {
					eventKind = "completed"
				}
				run.response.Entries = append(run.response.Entries,
					agendaEntry{Type: kind, instant: at},
					agendaEntry{Type: eventKind, instant: at},
				)
			}
			run.finish()
			m := run.response
			if len(m.Entries) != 1000 || !m.Truncated || m.WindowComplete {
				t.Fatalf("dated cap lost entries or completeness: entries=%d truncated=%v complete=%v", len(m.Entries), m.Truncated, m.WindowComplete)
			}
			if !m.EventsTruncated {
				t.Fatal("event cap not reported")
			}
			wantNext := ""
			if kind == "schedule" {
				wantNext = meAgendaNow.Add(500 * time.Second).Format(time.RFC3339)
			}
			if m.NextFrom != wantNext {
				t.Fatalf("next_from=%q, want %q", m.NextFrom, wantNext)
			}
			for i, e := range m.Entries {
				wantKind := kind
				if i%2 == 1 {
					wantKind = "created"
					if (i/2)%2 == 1 {
						wantKind = "completed"
					}
				}
				if e.Type != wantKind || !e.instant.Equal(meAgendaNow.Add(time.Duration(i/2)*time.Second)) {
					t.Fatalf("merged order at %d = %s %s", i, e.Type, e.instant)
				}
			}
		})
	}
}
