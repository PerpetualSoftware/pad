package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const activeScheduleMessage = "This item has a recurring schedule. Closing it stops its occurrences until it is reopened. Retry with force to close it anyway."

func recurringCloseHTTP(t *testing.T) *schedHTTP {
	t.Helper()
	f := endGuardHTTP(t)
	readCommitment(t, commitmentCall(f, "PATCH", map[string]any{"rrule": "FREQ=DAILY"}), 200)
	return f
}

func TestScheduleActiveUpdate(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"fields", "fields_patch"} {
		for _, status := range []string{"done", "cancelled"} {
			t.Run(field+"/"+status, func(t *testing.T) {
				f := recurringCloseHTTP(t)
				before, _ := f.srv.store.GetItem(f.item.ID)
				sc, _ := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
				path := strings.TrimSuffix(f.path(f.item), "/commitment")
				body := map[string]any{field: map[string]any{"status": status}, "title": "Must not land"}
				rr := f.as(f.token(f.owner, ""), "PATCH", path, body)
				d := f.expectErr(rr, 409, "schedule_active", "")
				e := schedMap(t, rr)["error"].(map[string]any)
				if e["message"] != activeScheduleMessage || d["schedule_id"] != sc.ID || d["next_at"] != *sc.NextAt {
					t.Fatalf("refusal = %s", rr.Body.String())
				}
				if _, ok := d["rule_summary"]; ok {
					t.Fatal("read exposes no rule summary")
				}
				after, _ := f.srv.store.GetItem(f.item.ID)
				if after.Seq != before.Seq || after.Fields != before.Fields || after.Title != before.Title {
					t.Fatal("refusal wrote item")
				}
				body["force"] = true
				rr = f.as(f.token(f.owner, ""), "PATCH", path, body)
				if rr.Code != 200 {
					t.Fatalf("force: %d %s", rr.Code, rr.Body.String())
				}
				afterSchedule, _ := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
				if afterSchedule.EndedAt != nil || afterSchedule.NextAt == nil || *afterSchedule.NextAt != *sc.NextAt {
					t.Fatalf("force changed schedule: %+v", afterSchedule)
				}
			})
		}
	}
}

func TestScheduleActiveExceptions(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"one-shot", "ended", "terminal-to-terminal", "non-terminal", "next-null"} {
		t.Run(mode, func(t *testing.T) {
			f := endGuardHTTP(t)
			if mode != "one-shot" {
				readCommitment(t, commitmentCall(f, "PATCH", map[string]any{"rrule": "FREQ=DAILY"}), 200)
			}
			status := "done"
			switch mode {
			case "ended":
				if _, err := f.srv.store.DB().Exec(`UPDATE item_schedules SET ended_at=?,next_at=NULL,next_wall=NULL WHERE item_id=?`, resolveX, f.item.ID); err != nil {
					t.Fatal(err)
				}
			case "terminal-to-terminal":
				if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"status": "done"}}); err != nil {
					t.Fatal(err)
				}
				status = "cancelled"
			case "non-terminal":
				status = "open"
			case "next-null":
				if _, err := f.srv.store.DB().Exec(`UPDATE item_schedules SET next_at=NULL,next_wall=NULL WHERE item_id=?`, f.item.ID); err != nil {
					t.Fatal(err)
				}
			}
			rr := f.as(f.token(f.owner, ""), "PATCH", strings.TrimSuffix(f.path(f.item), "/commitment"), map[string]any{"fields_patch": map[string]any{"status": status}})
			if mode == "next-null" {
				d := f.expectErr(rr, 409, "schedule_active", "")
				if v, ok := d["next_at"]; !ok || v != nil {
					t.Fatalf("next_at: %v", d)
				}
			} else if rr.Code != 200 {
				t.Fatalf("%s: %d %s", mode, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestScheduleActiveGuardOrder(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	parent, _ := seedParentAndChildren(t, srv, ws, []string{"open"})
	if _, err := srv.store.CreateSchedule(parent.WorkspaceID, parent.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/workspaces/" + ws + "/items/" + parent.Ref
	for _, code := range []string{"update_conflict", "open_children"} {
		body := map[string]any{"fields_patch": map[string]any{"status": "completed"}}
		if code == "update_conflict" {
			body["expected_seq"] = parent.Seq + 1000
		}
		rr := doRequest(srv, "PATCH", path, body)
		if rr.Code != 409 || !strings.Contains(rr.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("want %s: %d %s", code, rr.Code, rr.Body.String())
		}
	}
	rr := doRequest(srv, "PATCH", path, map[string]any{"fields_patch": map[string]any{"status": "completed"}, "force": true})
	if rr.Code != 200 {
		t.Fatalf("force both: %d %s", rr.Code, rr.Body.String())
	}
}

func TestScheduleActiveBulkUpdate(t *testing.T) {
	t.Parallel()
	f := recurringCloseHTTP(t)
	path := "/api/v1/workspaces/" + f.ws.Slug + "/items/bulk"
	body := map[string]any{"ids": []string{f.item.ID}, "op": "move", "status": "done"}
	rr := f.as(f.token(f.owner, ""), "POST", path, body)
	var res bulkItemsResponse
	parseJSON(t, rr, &res)
	if rr.Code != 200 || len(res.Updated) != 0 || len(res.Failed) != 1 || res.Failed[0].Code != "schedule_active" || res.Failed[0].Error != activeScheduleMessage {
		t.Fatalf("bulk: %d %s", rr.Code, rr.Body.String())
	}
	body["force"] = true
	rr = f.as(f.token(f.owner, ""), "POST", path, body)
	parseJSON(t, rr, &res)
	if rr.Code != 200 || len(res.Updated) != 1 || len(res.Failed) != 0 {
		t.Fatalf("force bulk: %d %s", rr.Code, rr.Body.String())
	}
}

func TestScheduleActiveReopenResumesAgenda(t *testing.T) {
	t.Parallel()
	f := recurringCloseHTTP(t)
	token := f.token(f.owner, "")
	path := strings.TrimSuffix(f.path(f.item), "/commitment")
	agenda := "/api/v1/workspaces/" + f.ws.Slug + "/agenda?from=2099-01-01T00:00:00Z&to=2099-01-03T00:00:00Z&tz=UTC"
	for _, status := range []string{"open", "done", "open"} {
		rr := f.as(token, "PATCH", path, map[string]any{"fields_patch": map[string]any{"status": status}, "force": status == "done"})
		if rr.Code != 200 {
			t.Fatalf("status: %d %s", rr.Code, rr.Body.String())
		}
		sc, err := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
		if err != nil || sc.EndedAt != nil || sc.NextAt == nil || *sc.NextAt != resolveX {
			t.Fatalf("schedule: %+v %v", sc, err)
		}
		rr = f.as(token, "GET", agenda, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("agenda: %d %s", rr.Code, rr.Body.String())
		}
		outstanding := strings.Contains(rr.Body.String(), `"state":"outstanding"`)
		if outstanding != (status == "open") {
			t.Fatalf("agenda after %s: %s", status, rr.Body.String())
		}
	}
}

func TestOpenChildrenScheduleActiveDetails(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"fields", "fields_patch", "bulk"} {
		for _, recurring := range []bool{false, true} {
			name := transport + "/children-only"
			if recurring {
				name = transport + "/both"
			}
			t.Run(name, func(t *testing.T) {
				srv := testServer(t)
				ws := createWSWithCollections(t, srv)
				parent, _ := seedParentAndChildren(t, srv, ws, []string{"open"})
				var schedule *models.ItemSchedule
				if recurring {
					var err error
					schedule, err = srv.store.CreateSchedule(parent.WorkspaceID, parent.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
					if err != nil {
						t.Fatal(err)
					}
				}
				path := "/api/v1/workspaces/" + ws + "/items/" + parent.Ref
				method := "PATCH"
				body := map[string]any{transport: map[string]any{"status": "completed"}}
				if transport == "bulk" {
					path = "/api/v1/workspaces/" + ws + "/items/bulk"
					method = "POST"
					body = map[string]any{"ids": []string{parent.ID}, "op": "move", "status": "completed"}
				}
				rr := doRequest(srv, method, path, body)
				var result map[string]any
				parseJSON(t, rr, &result)
				var refusal map[string]any
				if transport == "bulk" {
					failed, ok := result["failed"].([]any)
					if rr.Code != 200 || !ok || len(failed) != 1 {
						t.Fatalf("bulk refusal: %d %s", rr.Code, rr.Body.String())
					}
					refusal = failed[0].(map[string]any)
				} else {
					if rr.Code != 409 {
						t.Fatalf("refusal: %d %s", rr.Code, rr.Body.String())
					}
					refusal = result["error"].(map[string]any)
				}
				if refusal["code"] != "open_children" {
					t.Fatalf("refusal: %s", rr.Body.String())
				}
				details := refusal["details"].(map[string]any)
				active, present := details["schedule_active"]
				if recurring {
					d, ok := active.(map[string]any)
					if !ok || len(d) != 2 || d["schedule_id"] != schedule.ID || d["next_at"] != *schedule.NextAt {
						t.Fatalf("missing schedule warning: %s", rr.Body.String())
					}
				} else if present {
					t.Fatalf("unexpected schedule_active key: %s", rr.Body.String())
				}
				after, err := srv.store.GetItem(parent.ID)
				if err != nil || after.Seq != parent.Seq || after.Fields != parent.Fields {
					t.Fatalf("refusal wrote item: %+v %v", after, err)
				}
			})
		}
	}
}

// TASK-4601: a schedule with RDATEs and no RRULE still has future occurrences,
// so closing its item is refused the same way an RRULE schedule is.
func TestScheduleActiveRDateOnly(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"rdates-added", "rrule-cleared"} {
		t.Run(mode, func(t *testing.T) {
			f := endGuardHTTP(t)
			patch := map[string]any{"rdates": []string{"2099-01-05T09:00"}}
			if mode == "rrule-cleared" {
				readCommitment(t, commitmentCall(f, "PATCH", map[string]any{"rrule": "FREQ=DAILY", "rdates": []string{"2099-01-05T09:00"}}), 200)
				patch = map[string]any{"rrule": nil}
			}
			readCommitment(t, commitmentCall(f, "PATCH", patch), 200)
			sc, _ := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
			if sc.Rrule != nil && *sc.Rrule != "" || len(sc.RDates) != 1 || sc.EndedAt != nil {
				t.Fatalf("fixture schedule = %+v", sc)
			}
			path := strings.TrimSuffix(f.path(f.item), "/commitment")
			body := map[string]any{"fields_patch": map[string]any{"status": "done"}}
			rr := f.as(f.token(f.owner, ""), "PATCH", path, body)
			d := f.expectErr(rr, 409, "schedule_active", "")
			if d["schedule_id"] != sc.ID {
				t.Fatalf("refusal = %s", rr.Body.String())
			}
			body["force"] = true
			if rr = f.as(f.token(f.owner, ""), "PATCH", path, body); rr.Code != 200 {
				t.Fatalf("force: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// An RDATE that repeats the start, or is excluded, adds no second occurrence,
// so the schedule is still a one-shot and closing is not refused.
func TestScheduleActiveRDateOneShot(t *testing.T) {
	t.Parallel()
	for name, patch := range map[string]map[string]any{
		"rdate-is-start": {"rdates": []string{"2099-01-01T09:00"}},
		"rdate-excluded": {"rdates": []string{"2099-01-05T09:00"}, "exdates": []string{"2099-01-05T09:00"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := endGuardHTTP(t)
			readCommitment(t, commitmentCall(f, "PATCH", patch), 200)
			sc, _ := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
			if len(sc.RDates) != 1 || sc.EndedAt != nil {
				t.Fatalf("fixture schedule = %+v", sc)
			}
			rr := f.as(f.token(f.owner, ""), "PATCH", strings.TrimSuffix(f.path(f.item), "/commitment"), map[string]any{"fields_patch": map[string]any{"status": "done"}})
			if rr.Code != 200 {
				t.Fatalf("close: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}
