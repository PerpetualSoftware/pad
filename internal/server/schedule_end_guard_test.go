package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4537: a done or skip that ends an open item's schedule, and the
// Outstanding entry an ended schedule leaves.

const endGuardSchema = `{"fields":[{"key":"status","type":"select","options":["open","shipped","done","cancelled"],"default":"open","terminal_options":["done","shipped","cancelled"]}]}`

// endGuardHTTP is a schedHTTP whose item has a one-shot in 2099 and a status
// field with two delivering closes and one abandoning one.
func endGuardHTTP(t *testing.T) *schedHTTP {
	t.Helper()
	f := newSchedHTTP(t)
	coll, err := f.srv.store.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Chores", Slug: "chores", Prefix: "CH", Schema: endGuardSchema})
	if err != nil {
		t.Fatal(err)
	}
	it, err := f.srv.store.CreateItem(f.ws.ID, coll.ID, models.ItemCreate{Title: "Renew passport", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	f.coll, f.item = coll, it
	f.create(map[string]any{"roles": map[string]any{}, "dtstart": "2099-01-01T09:00", "tz": "UTC"})
	return f
}

func (f *schedHTTP) dismissPath(it *models.Item) string { return f.path(it) + "/dismiss-ended" }

func TestCommitmentResolveItemStillOpen(t *testing.T) {
	t.Parallel()
	f := endGuardHTTP(t)
	for _, state := range []string{"done", "skip"} {
		rr := f.resolve(map[string]any{"occurrence_at": resolveX, "state": state, "comment": "finished"})
		d := f.expectErr(rr, http.StatusConflict, "item_still_open", "")
		e, _ := schedMap(t, rr)["error"].(map[string]any)
		if e["message"] != itemStillOpenMessage || !strings.Contains(itemStillOpenMessage, "Close the item first") || !strings.Contains(itemStillOpenMessage, "end_schedule") {
			t.Fatalf("message = %v", e["message"])
		}
		opts, _ := d["close_options"].([]any)
		if d["state"] != state || d["occurrence_at"] != resolveX || d["done_field"] != "status" || d["item_status"] != "open" || d["ack_available"] != false ||
			len(opts) != 2 || opts[0] != "shipped" || opts[1] != "done" {
			t.Fatalf("details = %v", d)
		}
	}
	if f.occurrences() != 0 || f.comments() != 0 || f.count(`SELECT COUNT(*) FROM activities WHERE document_id=? AND action='schedule_resolved'`, f.item.ID) != 0 {
		t.Fatal("a refused resolve wrote")
	}

	// ack_available follows the occurrence: fired and not yet acknowledged.
	if _, err := f.srv.store.DB().Exec(`UPDATE item_reminders SET fired_at=? WHERE item_id=?`, resolveX, f.item.ID); err != nil {
		t.Fatal(err)
	}
	d := f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done"}), http.StatusConflict, "item_still_open", "")
	if d["ack_available"] != true {
		t.Fatalf("fired = %v", d)
	}
	if rr := f.resolve(map[string]any{"occurrence_at": resolveX, "state": "ack"}); rr.Code != http.StatusOK {
		t.Fatalf("ack = %d %s", rr.Code, rr.Body.String())
	}
	d = f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done"}), http.StatusConflict, "item_still_open", "")
	if d["ack_available"] != false {
		t.Fatalf("acknowledged = %v", d)
	}

	// end_schedule: type and combinations.
	f.expectErr(f.resolve(`{"occurrence_at":"`+resolveX+`","state":"done","end_schedule":"yes"}`), 400, "invalid_body", "")
	until := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	for _, state := range []string{"snooze", "move"} {
		d := f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": state, "until": until, "end_schedule": true}), 400, "validation_error", "combination")
		if d["field"] != "end_schedule" {
			t.Fatalf("%s details = %v", state, d)
		}
	}
	for _, v := range []any{true, false, nil} {
		d := f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": "ack", "end_schedule": v}), 400, "validation_error", "combination")
		if d["field"] != "end_schedule" {
			t.Fatalf("ack details = %v", d)
		}
	}
	// false is the default: still refused.
	f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done", "end_schedule": false}), http.StatusConflict, "item_still_open", "")

	rr := f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done", "end_schedule": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("consented = %d %s", rr.Code, rr.Body.String())
	}
	m := schedMap(t, rr)
	res, _ := m["resolution"].(map[string]any)
	view, _ := m["commitment"].(map[string]any)
	if res["item_still_open"] != true || view["ended_at"] == nil || view["ended_dismissed_at"] != nil {
		t.Fatalf("consented body = %s", rr.Body.String())
	}
}

// A done that ends a recurring schedule's last instance on a closed item, and
// a done with a next occurrence, carry no item_still_open.
func TestCommitmentResolveItemStillOpenAbsentWhenUnguarded(t *testing.T) {
	t.Parallel()
	f := endGuardHTTP(t)
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE items SET fields=? WHERE id=?`), `{"status":"done"}`, f.item.ID); err != nil {
		t.Fatal(err)
	}
	rr := f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done", "end_schedule": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("closed = %d %s", rr.Code, rr.Body.String())
	}
	if res, _ := schedMap(t, rr)["resolution"].(map[string]any); res["item_still_open"] != nil {
		t.Fatalf("closed resolution = %v", res)
	}
}

func TestDismissEndedOracleAndGates(t *testing.T) {
	t.Parallel()
	f := endGuardHTTP(t)
	owner := f.token(f.owner, "")
	f.expectErr(f.as(owner, "POST", f.dismissPath(f.item), nil), http.StatusConflict, "not_ended", "")
	if rr := f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done", "end_schedule": true}); rr.Code != http.StatusOK {
		t.Fatalf("end = %d %s", rr.Code, rr.Body.String())
	}

	// Hidden and missing are the same 404.
	guest := mustUser(t, f.srv, "dismiss-guest@example.com", "dismissguest", "")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, guest.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, guest.ID, "specific", []string{}); err != nil {
		t.Fatal(err)
	}
	gtok := f.token(guest, "")
	hidden := f.as(gtok, "POST", f.dismissPath(f.item), nil)
	missing := f.as(gtok, "POST", strings.Replace(f.dismissPath(f.item), f.item.Slug, "NOPE-99999", 1), nil)
	if hidden.Code != http.StatusNotFound || hidden.Body.String() != missing.Body.String() {
		t.Fatalf("hidden %d %s / missing %d %s", hidden.Code, hidden.Body.String(), missing.Code, missing.Body.String())
	}
	viewer := f.member("dismiss-viewer@example.com", "viewer")
	if rr := f.as(f.token(viewer, ""), "POST", f.dismissPath(f.item), nil); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer = %d", rr.Code)
	}
	f.expectErr(f.as(owner, "POST", f.dismissPath(f.item), `{"x":1}`), 400, "invalid_body", "")

	editor := f.member("dismiss-editor@example.com", "editor")
	rr := f.as(f.token(editor, ""), "POST", f.dismissPath(f.item), map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("dismiss = %d %s", rr.Code, rr.Body.String())
	}
	view, _ := schedMap(t, rr)["commitment"].(map[string]any)
	if view["ended_dismissed_at"] == nil {
		t.Fatalf("dismiss body = %s", rr.Body.String())
	}
	if rr := f.as(owner, "POST", f.dismissPath(f.item), nil); rr.Code != http.StatusOK {
		t.Fatalf("again = %d %s", rr.Code, rr.Body.String())
	}
	if n := f.count(`SELECT COUNT(*) FROM activities WHERE document_id=? AND action='schedule_end_dismissed'`, f.item.ID); n != 1 {
		t.Fatalf("activities = %d, want one for the change only", n)
	}

	// No schedule: 404. Archived: 409.
	bare := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "No schedule")
	f.expectErr(f.as(owner, "POST", f.dismissPath(bare), nil), http.StatusNotFound, "not_found", "")
	if err := f.srv.store.DeleteItem(f.item.ID); err != nil {
		t.Fatal(err)
	}
	f.expectErr(f.as(owner, "POST", f.dismissPath(f.item), nil), http.StatusConflict, "archived", "")
}

// endedItem gives w an open item whose one-shot the owner ended with consent.
func (w *bootAgendaWorld) endedItem(t *testing.T, title string) *models.Item {
	t.Helper()
	it := w.sched(t, w.owner, title, "2031-03-10T09:00", "both")
	if _, err := w.srv.store.ResolveOccurrence(w.ws.ID, it.ID, store.ResolveInput{OccurrenceAt: time.Date(2031, 3, 10, 9, 0, 0, 0, time.UTC), State: "done", Actor: "user", EndSchedule: true}); err != nil {
		t.Fatal(err)
	}
	return it
}

func agendaRowsFor(m map[string]any, group, ref string) []map[string]any {
	var out []map[string]any
	rows, _ := m[group].([]any)
	for _, v := range rows {
		if r, _ := v.(map[string]any); r != nil && r["item_ref"] == ref {
			out = append(out, r)
		}
	}
	return out
}

func TestAgendaScheduleEndedOutstanding(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
	ended := w.endedItem(t, "ended")
	closed := w.endedItem(t, "closed")
	if _, err := w.srv.store.DB().Exec(w.srv.store.D().Rebind(`UPDATE items SET fields=? WHERE id=?`), `{"status":"done"}`, closed.ID); err != nil {
		t.Fatal(err)
	}
	check := func(stage string, wantEntry bool) {
		t.Helper()
		for name, m := range map[string]map[string]any{"workspace": w.ok(t, w.owner, agendaWindow), "me": meAgendaOK(t, w.agendaWorld, w.owner, agendaWindow)} {
			rows := agendaRowsFor(m, "priority_list", ended.Ref)
			if wantEntry {
				if len(rows) != 1 || rows[0]["reason"] != "schedule_ended" || rows[0]["type"] != "item" || rows[0]["ended_at"] == nil || rows[0]["schedule_id"] == nil || rows[0]["at"] != nil {
					t.Fatalf("%s %s: priority rows = %v", stage, name, rows)
				}
			} else if len(rows) != 0 {
				t.Fatalf("%s %s: dismissed entry still listed: %v", stage, name, rows)
			}
			if m["priority_list_complete"] != true {
				t.Fatalf("%s %s: priority_list_complete = %v", stage, name, m["priority_list_complete"])
			}
			if len(agendaRowsFor(m, "priority_list", closed.Ref))+len(agendaRowsFor(m, "no_date", closed.Ref)) != 0 {
				t.Fatalf("%s %s: a closed item is listed", stage, name)
			}
			noDate := agendaRowsFor(m, "no_date", ended.Ref)
			if name == "me" && !wantEntry && (len(noDate) != 1 || noDate[0]["reason"] != "unplaced") {
				t.Fatalf("%s me: dismissed item not in no_date: %v", stage, m["no_date"])
			}
			if name == "workspace" && len(noDate) != 0 {
				t.Fatalf("%s workspace: no_date = %v", stage, noDate)
			}
		}
		b := w.bootstrap(t, w.tok, nil).Agenda
		found := false
		for _, e := range b.PriorityList {
			found = found || (e.ItemRef == ended.Ref && e.Reason == "schedule_ended")
		}
		if found != wantEntry {
			t.Fatalf("%s bootstrap: priority list = %+v", stage, b.PriorityList)
		}
	}
	check("ended", true)
	if _, err := w.srv.store.DismissEndedSchedule(w.ws.ID, ended.ID); err != nil {
		t.Fatal(err)
	}
	check("dismissed", false)
}

func TestAgendaScheduleEndedWithoutClosingValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, schema string
		closable     bool
	}{
		{"no done field", `{"fields":[]}`, false},
		{"no terminal option", `{"fields":[{"key":"status","type":"select","options":["open","published"]}]}`, false},
		{"number done field", `{"fields":[{"key":"status","type":"number"}]}`, false},
		{"abandoning only", `{"fields":[{"key":"status","type":"select","options":["open","cancelled"]}]}`, true},
		{"closable", endGuardSchema, true},
	} {
		for _, ending := range []string{"lapse", "timing edit"} {
			t.Run(tc.name+"/"+ending, func(t *testing.T) {
				w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
				it := w.sched(t, w.owner, "ended", "2031-03-10T09:00", "both")
				if _, err := w.srv.store.DB().Exec(`UPDATE collections SET schema=? WHERE id=?`, tc.schema, it.CollectionID); err != nil {
					t.Fatal(err)
				}
				if ending == "lapse" {
					if _, err := w.srv.store.DB().Exec(`UPDATE item_schedules SET roles_origin='native', deadline_days=0, deadline_secs=0, deadline_behavior='lapses' WHERE item_id=?`, it.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := w.srv.store.EvaluateDeadlines("2031-03-13T12:00:00Z", 0); err != nil {
						t.Fatal(err)
					}
				} else {
					past := "2000-01-01T09:00"
					if _, err := w.srv.store.UpdateSchedule(w.ws.ID, it.ID, models.ScheduleUpdate{Dtstart: &past}); err != nil {
						t.Fatal(err)
					}
				}
				sc, err := w.srv.store.GetSchedule(w.ws.ID, it.ID)
				if err != nil || sc == nil || sc.EndedAt == nil || sc.EndedDismissedAt != nil {
					t.Fatalf("want an undismissed ending: %+v, %v", sc, err)
				}
				for name, m := range map[string]map[string]any{"workspace": w.ok(t, w.owner, agendaWindow), "me": meAgendaOK(t, w.agendaWorld, w.owner, agendaWindow)} {
					rows := agendaRowsFor(m, "priority_list", it.Ref)
					if tc.closable {
						if len(rows) != 1 || rows[0]["reason"] != "schedule_ended" {
							t.Errorf("%s: missing ended entry: %v", name, rows)
						}
					} else if len(rows) != 0 {
						t.Errorf("%s: item cannot close but is Outstanding: %v", name, rows)
					}
					noDate := agendaRowsFor(m, "no_date", it.Ref)
					if name == "me" && !tc.closable {
						if len(noDate) != 1 || noDate[0]["reason"] != "unplaced" {
							t.Errorf("%s: want undated work: %v", name, noDate)
						}
					} else if len(noDate) != 0 {
						t.Errorf("%s: unexpected undated work: %v", name, noDate)
					}
					for _, flag := range []string{"priority_list_complete", "waiting_on_complete", "no_date_complete"} {
						if m[flag] != true {
							t.Errorf("%s: %s = %v", name, flag, m[flag])
						}
					}
				}
			})
		}
	}
}

func TestAgendaScheduleEndedOAuthAllowlist(t *testing.T) {
	t.Parallel()
	w := newBootAgendaWorld(t, "2031-03-13T12:00:00Z")
	ended := w.endedItem(t, "ended")
	other := mustWorkspace(t, w.srv, "Elsewhere", w.owner.ID)
	m := meAgendaOK(t, w.agendaWorld, w.owner, agendaWindow, func(c context.Context) context.Context {
		return WithTokenAllowedWorkspaces(c, []string{other.Slug})
	})
	for _, group := range []string{"priority_list", "no_date", "waiting_on"} {
		if rows := agendaRowsFor(m, group, ended.Ref); len(rows) != 0 {
			t.Fatalf("%s leaked past the allow-list: %v", group, rows)
		}
	}
	m = meAgendaOK(t, w.agendaWorld, w.owner, agendaWindow, func(c context.Context) context.Context {
		return WithTokenAllowedWorkspaces(c, []string{w.ws.Slug})
	})
	if rows := agendaRowsFor(m, "priority_list", ended.Ref); len(rows) != 1 {
		t.Fatalf("allowed workspace: %v", m["priority_list"])
	}
}

func TestCommitmentResolveDoneFilterCloseOptions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, schema, settings string }{
		{"text status", `{"fields":[{"key":"status","type":"text"}]}`, `{}`},
		{"bad settings", `{"fields":[{"key":"status","type":"select","options":["open","done"]}]}`, `{"board_group_by":42}`},
		{"bad schema", `{"fields":42}`, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := endGuardHTTP(t)
			if _, err := f.srv.store.DB().Exec(`UPDATE collections SET schema=?, settings=? WHERE id=?`, tc.schema, tc.settings, f.coll.ID); err != nil {
				t.Fatal(err)
			}
			d := f.expectErr(f.resolve(map[string]any{"occurrence_at": resolveX, "state": "done"}), http.StatusConflict, "item_still_open", "")
			opts, _ := d["close_options"].([]any)
			if d["done_field"] != "status" || len(opts) == 0 || opts[0] != "done" {
				t.Fatalf("details = %v", d)
			}
			if f.occurrences() != 0 {
				t.Fatal("refusal wrote an occurrence")
			}
		})
	}
}
