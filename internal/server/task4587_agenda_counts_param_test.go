package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestTask4587AgendaCountsModes(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	meAgendaFacts(t, w, "tasks", "Scheduled")
	open := w.item(t, "tasks", "Open work", `{"status":"open"}`, nil)
	meAgendaExec(t, w, `UPDATE items SET assigned_user_id=? WHERE id=?`, w.owner.ID, open.ID)
	baseline := meAgendaOK(t, w, w.owner, agendaWindow)
	if countsOf(baseline) == "" || len(entriesOf(baseline)) == 0 || len(baseline["no_date"].([]any)) == 0 {
		t.Fatalf("fixture must contain counts, scheduled entries and open work: %v", baseline)
	}
	for _, mode := range []string{"include", "%20include%20", "omit", "only", "%20only%20"} {
		t.Run(mode, func(t *testing.T) {
			got := meAgendaOK(t, w, w.owner, agendaWindow+"&counts="+mode)
			switch mode {
			case "include", "%20include%20":
				if !reflect.DeepEqual(got, baseline) {
					t.Fatalf("include differs from default: %v / %v", got, baseline)
				}
			case "omit":
				for _, key := range []string{"counts", "counts_truncated"} {
					if _, ok := got[key]; ok {
						t.Errorf("omit contains %s", key)
					}
				}
				want := make(map[string]any)
				for key, value := range baseline {
					if key != "counts" && key != "counts_truncated" {
						want[key] = value
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("omit changed list: %v / %v", got, want)
				}
			default:
				want := make(map[string]any)
				for _, key := range []string{"from", "to", "tz", "timezone_note", "counts", "counts_truncated"} {
					if value, ok := baseline[key]; ok {
						want[key] = value
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("only response: %v, want %v", got, want)
				}
			}
		})
	}
}

func TestTask4587AgendaCountsValidation(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	for _, query := range []string{"counts=bogus", "counts=", "counts=only&counts=omit", "counts=bogus&workspace=" + uuid.NewString()} {
		t.Run(query, func(t *testing.T) {
			rr := meAgendaGet(t, w, w.owner, agendaWindow+"&"+query)
			var body struct {
				Error struct {
					Code    string
					Details struct{ Field, Code string }
				}
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rr.Code != http.StatusBadRequest || body.Error.Code != "validation_error" || body.Error.Details.Field != "counts" || body.Error.Details.Code != "format" {
				t.Fatalf("invalid counts: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
	if rr := meAgendaGet(t, w, w.owner, agendaWindow+"&counts=only&include=bogus"); rr.Code != http.StatusBadRequest {
		t.Fatalf("include must still be validated: %d", rr.Code)
	}
}

func TestTask4587AgendaCountsOnlyUnavailable(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	stranger := mustUser(t, w.srv, "task4587@example.com", "task4587", "")
	normal := meAgendaGet(t, w, stranger, agendaWindow+"&workspace="+w.ws.ID)
	only := meAgendaGet(t, w, stranger, agendaWindow+"&workspace="+w.ws.ID+"&counts=only")
	if normal.Code != http.StatusNotFound || only.Code != normal.Code || only.Body.String() != normal.Body.String() {
		t.Fatalf("unavailable differs: %d %s / %d %s", normal.Code, normal.Body.String(), only.Code, only.Body.String())
	}
}

func TestTask4587AgendaCountsOnlyBudget(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	meAgendaFacts(t, w, "tasks", "One")
	meAgendaFacts(t, w, "tasks", "Two")
	w.srv.agendaCountsLimit = 1
	got := meAgendaOK(t, w, w.owner, agendaWindow+"&counts=only")
	if got["counts_truncated"] != true {
		t.Fatalf("missing truncation: %v", got)
	}
	if _, ok := got["counts"]; ok {
		t.Fatalf("partial counts: %v", got)
	}
	if _, ok := got["entries"]; ok {
		t.Fatalf("only includes entries: %v", got)
	}
}
