package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestAckItemReminderCandidates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		fired, armed, acked int
		managed             bool
		status              int
		code                string
	}{
		{"one", 1, 1, 1, false, 200, ""},
		{"zero", 0, 0, 0, false, 404, "not_found"},
		{"two", 2, 0, 0, false, 409, "ambiguous_reminder"},
		{"armed only", 0, 1, 0, false, 404, "not_found"},
		{"acked only", 0, 0, 1, false, 404, "not_found"},
		{"managed only", 0, 0, 0, true, 404, "not_found"},
		{"one and managed", 1, 0, 0, true, 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			slug := createWSWithCollections(t, srv)
			item := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Ack by ref"})
			ws, err := srv.store.GetWorkspaceBySlug(slug)
			if err != nil {
				t.Fatal(err)
			}
			var candidates []string
			for i := 0; i < tc.fired+tc.acked; i++ {
				r := armViaAPI(t, srv, slug, item, pastInstant)
				if _, err := srv.store.DB().Exec(`UPDATE item_reminders SET fired_at=? WHERE id=?`, pastInstant, r.ID); err != nil {
					t.Fatal(err)
				}
				if i < tc.fired {
					candidates = append(candidates, r.ID)
				} else if _, err := srv.store.AckReminder(ws.ID, r.ID); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < tc.armed; i++ {
				armViaAPI(t, srv, slug, item, futureInstant)
			}
			if tc.managed {
				sc, err := srv.store.CreateSchedule(ws.ID, item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := srv.store.DB().Exec(`UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, pastInstant, sc.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := srv.store.ListRemindersForItem(ws.ID, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Ref+"/reminders/ack", nil)
			if rr.Code != tc.status {
				t.Fatalf("got %d %s, want %d", rr.Code, rr.Body.String(), tc.status)
			}
			if tc.code != "" {
				var body struct {
					Error struct {
						Code, Message string
						Details       struct {
							IDs []string `json:"reminder_ids"`
						}
					}
				}
				parseJSON(t, rr, &body)
				if body.Error.Code != tc.code {
					t.Errorf("code = %s", body.Error.Code)
				}
				if tc.status == 404 && body.Error.Message != "No fired, unacknowledged reminder on "+item.Ref {
					t.Errorf("message = %q", body.Error.Message)
				}
				if tc.status == 409 {
					if !strings.Contains(body.Error.Message, "2") || !strings.Contains(body.Error.Message, "id") {
						t.Errorf("message = %q", body.Error.Message)
					}
					ids, _ := json.Marshal(body.Error.Details.IDs)
					for _, id := range candidates {
						if !strings.Contains(string(ids), id) {
							t.Errorf("missing candidate %s in %s", id, ids)
						}
					}
				}
			} else {
				var got models.Reminder
				parseJSON(t, rr, &got)
				if got.ID != candidates[0] || got.AckedAt == nil {
					t.Errorf("acked = %+v", got)
				}
			}
			for _, old := range before {
				got, err := srv.store.GetReminder(ws.ID, old.ID)
				if err != nil {
					t.Fatal(err)
				}
				wantAcked := old.AckedAt != nil || (tc.status == 200 && old.ID == candidates[0])
				if (got.AckedAt != nil) != wantAcked {
					t.Errorf("reminder %s acked = %v, want %v", got.ID, got.AckedAt, wantAcked)
				}
			}
		})
	}
}

func TestAckItemReminderPermissions(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	owner := mustUser(t, srv, "ack-owner@example.com", "ackowner", "")
	ws := mustWorkspace(t, srv, "Ack permissions", owner.ID)
	coll := mustCollection(t, srv, ws.ID, "Tasks")
	visible := mustItem(t, srv, ws.ID, coll.ID, "Visible")
	hidden := mustItem(t, srv, ws.ID, coll.ID, "Hidden")
	guest := mustUser(t, srv, "ack-guest@example.com", "ackguest", "")
	if _, err := srv.store.CreateItemGrant(ws.ID, visible.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	r, err := srv.store.CreateReminder(ws.ID, visible.ID, pastInstant)
	if err != nil {
		t.Fatal(err)
	}
	srv.runReminderTick()
	tok, err := srv.store.CreateSession(guest.ID, testSessionUA, "192.0.2.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/workspaces/" + ws.Slug + "/items/"
	unknown := reminderWriteAs(srv, "POST", base+"TASK-999999/reminders/ack", nil, tok)
	secret := reminderWriteAs(srv, "POST", base+hidden.ID+"/reminders/ack", nil, tok)
	if secret.Code != http.StatusNotFound || secret.Code != unknown.Code || secret.Body.String() != unknown.Body.String() {
		t.Fatalf("hidden = %d %s, unknown = %d %s", secret.Code, secret.Body.String(), unknown.Code, unknown.Body.String())
	}
	denied := reminderWriteAs(srv, "POST", base+visible.ID+"/reminders/ack", nil, tok)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("view grant = %d %s", denied.Code, denied.Body.String())
	}
	got, err := srv.store.GetReminder(ws.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AckedAt != nil {
		t.Fatal("refused ack changed reminder")
	}
}

func TestAckItemReminderArchived(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Archived"})
	armViaAPI(t, srv, slug, item, pastInstant)
	srv.runReminderTick()
	base := "/api/v1/workspaces/" + slug
	if rr := doRequest(srv, "DELETE", base+"/items/"+item.Ref, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("archive: %d %s", rr.Code, rr.Body.String())
	}
	byCreate := doRequest(srv, "POST", base+"/items/"+item.Ref+"/reminders", nil)
	byRef := doRequest(srv, "POST", base+"/items/"+item.Ref+"/reminders/ack", nil)
	if byRef.Code != 409 || byRef.Body.String() != byCreate.Body.String() {
		t.Fatalf("ack = %d %s, create = %d %s", byRef.Code, byRef.Body.String(), byCreate.Code, byCreate.Body.String())
	}
}
