package server

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func TestTASK4423_R05Notices(t *testing.T) {
	t.Run("graph routes", testGraphRouteNotices)
	for _, kind := range []string{"deadline_approval_requested", "commitment_moved", "commitment_moved_past_deadline", "commitment_gate_opened"} {
		t.Run(kind, func(t *testing.T) {
			needsBackends(t, func(t *testing.T, srv *Server) {
				for _, mode := range []string{"assigned", "owner", "lost-access", "reassigned", "preference-off"} {
					t.Run(mode, func(t *testing.T) {
						f := newPdx(t, srv)
						tok := f.bobSession()
						n := store.Notification{Kind: kind, UserID: f.bob.ID, WorkspaceID: f.ws.ID, ItemID: f.item.ID}
						set := models.DefaultNotificationSettings()
						if reason := kindEnabled(set, n); reason != "" {
							t.Fatalf("enabled: %s", reason)
						}
						set.NotifyAssigned = false
						if reason := kindEnabled(set, n); reason != skipSettingOff {
							t.Fatalf("disabled: %s", reason)
						}
						if mode == "owner" {
							f.exec(`UPDATE items SET assigned_user_id=NULL WHERE id=?`, f.item.ID)
							f.exec(`UPDATE workspaces SET owner_id=? WHERE id=?`, f.bob.ID, f.ws.ID)
						}
						id := f.row(kind)
						if mode == "lost-access" {
							must(t, srv.store.SetMemberCollectionAccess(f.ws.ID, f.bob.ID, "specific", []string{}))
						}
						if mode == "reassigned" {
							f.exec(`UPDATE items SET assigned_user_id=? WHERE id=?`, f.owner.ID, f.item.ID)
						}
						if mode == "preference-off" {
							f.settings(func(s *models.NotificationSettings) { s.NotifyAssigned = false })
						}
						_, reason, err := srv.notificationAccess(nil, n)
						if err != nil {
							t.Fatal(err)
						}
						if (mode == "lost-access" || mode == "reassigned") != (reason != "") {
							t.Fatalf("access=%s", reason)
						}
						rr := getNotif(srv, tok, id)
						if reason != "" {
							sameBytes(t, "lost eligibility", rr, getNotif(srv, tok, "missing"))
						} else if rr.Code != http.StatusOK {
							t.Fatalf("read: %d %s", rr.Code, rr.Body.String())
						}
						f.tick()
						state := f.state(id)
						want := "sent"
						if reason != "" || mode == "preference-off" {
							want = "skipped"
						}
						if state.State != want {
							t.Fatalf("dispatch=%+v want %s", state, want)
						}
						if _, ok := kindPriority[kind]; !ok {
							t.Fatal("kind missing from ordering")
						}
					})
				}
			})
		})
	}
}

func TestTASK4423_R15RefusalNotice(t *testing.T) {
	for _, door := range []string{"resolve", "renegotiate"} {
		t.Run(door, func(t *testing.T) {
			roles := models.CommitmentRolesInput{}
			if door == "renegotiate" {
				roles = graphHard()
			}
			f := newGraphHTTP(t, roles, graphHard())
			pr := "target"
			if door == "renegotiate" {
				pr = "deadline"
			}
			f.link(0, 1, pr, "deadline", "")
			g := f.guest(0)
			tok := f.token(g, "")
			if _, err := f.srv.store.CreateSession(f.owner.ID, "ios", "", "", time.Hour); err != nil {
				t.Fatal(err)
			}
			var sid string
			if err := f.srv.store.DB().QueryRow(`SELECT id FROM sessions WHERE user_id=? LIMIT 1`, f.owner.ID).Scan(&sid); err != nil {
				t.Fatal(err)
			}
			if _, err := f.srv.store.RegisterPushDevice(f.owner.ID, sid, "graph-notice", "ios", "graph-device", ""); err != nil {
				t.Fatal(err)
			}
			to := time.Now().UTC().Truncate(time.Second).Add(25 * 24 * time.Hour).Format(time.RFC3339)
			body := map[string]any{"state": "move", "occurrence_at": f.dates[0], "until": to}
			if door == "renegotiate" {
				preview := f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/renegotiate", map[string]any{"deadline": to, "reason": "Replan", "preview": true})
				if preview.Code != 200 {
					t.Fatalf("owner preview: %d %s", preview.Code, preview.Body.String())
				}
				revision, _ := schedMap(t, preview)["revision"].(string)
				body = map[string]any{"deadline": to, "reason": "Replan", "revision": revision}
			}
			var response string
			for n := 0; n < 2; n++ {
				rr := f.as(tok, "POST", f.path(f.item)+"/"+door, body)
				f.expectErr(rr, 409, "deadline_approval_required", "")
				if n == 0 {
					response = rr.Body.String()
				} else if rr.Body.String() != response {
					t.Fatal("dedupe changed refusal")
				}
			}
			if n := f.count(`SELECT COUNT(*) FROM user_notifications WHERE user_id=? AND kind='deadline_approval_requested'`, f.owner.ID); n != 1 {
				t.Fatalf("approval notices=%d, want 1", n)
			}
			if _, err := f.srv.store.DB().Exec(`DELETE FROM push_devices WHERE user_id=?`, f.owner.ID); err != nil {
				t.Fatal(err)
			}
			rr := f.as(tok, "POST", f.path(f.item)+"/"+door, body)
			if rr.Body.String() != response {
				t.Fatal("no recipient changed refusal")
			}
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previousLogger)
			f.srv.enqueueCommitmentRefusal = func(string, models.LinkedMoveInput) error { return errors.New("hidden-sentinel-id") }
			rr = f.as(tok, "POST", f.path(f.item)+"/"+door, body)
			if rr.Body.String() != response {
				t.Fatal("enqueue failure changed refusal")
			}
			if strings.Contains(logs.String(), "hidden-sentinel-id") {
				t.Fatal("notice failure logged private error text")
			}
		})
	}
}

func graphPushDevice(t *testing.T, f *graphHTTP, user *models.User) {
	t.Helper()
	_, err := f.srv.store.CreateSession(user.ID, "ios", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var sid string
	if err := f.srv.store.DB().QueryRow(`SELECT id FROM sessions WHERE user_id=? LIMIT 1`, user.ID).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.RegisterPushDevice(user.ID, sid, "graph-route-notice", "ios", "graph-route-device", ""); err != nil {
		t.Fatal(err)
	}
}
func testGraphRouteNotices(t *testing.T) {
	t.Run("reassigned root after refusal", func(t *testing.T) {
		f := newGraphHTTP(t, graphHard(), graphHard())
		f.link(0, 1, "deadline", "deadline", "")
		g := f.guest(0)
		if _, err := f.srv.store.DB().Exec(`UPDATE items SET assigned_user_id=? WHERE id=?`, g.ID, f.item.ID); err != nil {
			t.Fatal(err)
		}
		graphPushDevice(t, f, f.owner)
		f.srv.enqueueCommitmentRefusal = func(ws string, in models.LinkedMoveInput) error {
			if _, err := f.srv.store.DB().Exec(`UPDATE items SET assigned_user_id=? WHERE id=?`, f.owner.ID, f.item.ID); err != nil {
				return err
			}
			return f.srv.store.EnqueueLinkedDeadlineApprovalNotice(ws, in)
		}
		rr := f.as(f.token(g, ""), "POST", f.path(f.item)+"/move", map[string]any{"role": "deadline", "to": time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339), "reason": "Replan"})
		f.expectErr(rr, 409, "deadline_approval_required", "")
		if n := f.count(`SELECT COUNT(*) FROM user_notifications WHERE kind='deadline_approval_requested'`); n != 0 {
			t.Fatalf("stale assignee refusal enqueued %d notices", n)
		}
	})

	t.Run("hidden bound and history", func(t *testing.T) {
		f := newGraphHTTP(t, models.CommitmentRolesInput{}, graphHard())
		f.link(0, 1, "target", "target", "")
		g := f.guest(0)
		assignee := mustUser(t, f.srv, "hidden-assignee@example.com", "hiddenassignee", "")
		if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.items[1].ID, assignee.ID, "view", f.owner.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.store.DB().Exec(`UPDATE items SET assigned_user_id=? WHERE id=?`, assignee.ID, f.items[1].ID); err != nil {
			t.Fatal(err)
		}
		graphPushDevice(t, f, assignee)
		to := time.Now().UTC().Truncate(time.Second).Add(35 * 24 * time.Hour).Format(time.RFC3339)
		rr := f.as(f.token(g, ""), "POST", f.path(f.item)+"/move", map[string]any{"role": "target", "to": to, "reason": "private-parent-reason"})
		if rr.Code != 200 {
			t.Fatalf("hidden crossing: %d %s", rr.Code, rr.Body.String())
		}
		for _, key := range []string{"violations", "left_in_place", "renegotiates_deadline"} {
			if _, ok := schedMap(t, rr)[key]; ok {
				t.Fatalf("hidden crossing disclosed %s", key)
			}
		}
		if n := f.count(`SELECT COUNT(*) FROM user_notifications WHERE user_id=? AND kind='commitment_moved_past_deadline'`, assignee.ID); n != 1 {
			t.Fatalf("past deadline notices=%d", n)
		}
		view := f.as(f.token(assignee, ""), "GET", f.path(f.items[1]), nil)
		if view.Code != 200 {
			t.Fatalf("child view: %d %s", view.Code, view.Body.String())
		}
		if !strings.Contains(view.Body.String(), `"conflicts":[{`) || !strings.Contains(view.Body.String(), `"change_type":"move"`) {
			t.Fatalf("missing conflict/history: %s", view.Body.String())
		}
		for _, private := range []string{f.item.ID, f.item.Slug, "private-parent-reason"} {
			if strings.Contains(view.Body.String(), private) {
				t.Fatalf("child view disclosed %q", private)
			}
		}
		var payload string
		if err := f.srv.store.DB().QueryRow(`SELECT item_id FROM user_notifications WHERE user_id=? AND kind='commitment_moved_past_deadline'`, assignee.ID).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		if payload != f.items[1].ID {
			t.Fatal("notice subject is not child")
		}
	})
	var baseline string
	for _, n := range []int{1, 3} {
		t.Run(fmt.Sprintf("refusal/%d dependents", n), func(t *testing.T) {
			roles := []models.CommitmentRolesInput{{}}
			for i := 0; i < n; i++ {
				roles = append(roles, graphHard())
			}
			f := newGraphHTTP(t, roles...)
			for i := 1; i <= n; i++ {
				f.link(0, i, "target", "deadline", "")
			}
			g := f.guest(0)
			tok := f.token(g, "")
			graphPushDevice(t, f, f.owner)
			body := map[string]any{"role": "target", "to": time.Now().UTC().Add(35 * 24 * time.Hour).Format(time.RFC3339), "reason": "Private"}
			for _, mode := range []string{"success", "dedupe", "no-recipient", "failure"} {
				if mode == "no-recipient" {
					if _, err := f.srv.store.DB().Exec(`DELETE FROM push_devices`); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "failure" {
					f.srv.enqueueCommitmentRefusal = func(string, models.LinkedMoveInput) error { return errors.New("private-error") }
				}
				rr := f.as(tok, "POST", f.path(f.item)+"/move", body)
				f.expectErr(rr, 409, "deadline_approval_required", "")
				if baseline == "" {
					baseline = rr.Body.String()
				}
				if baseline != rr.Body.String() {
					t.Fatalf("%s changed refusal bytes", mode)
				}
				if mode == "success" || mode == "dedupe" {
					if count := f.count(`SELECT COUNT(*) FROM user_notifications WHERE kind='deadline_approval_requested'`); count != 1 {
						t.Fatalf("approval notices=%d", count)
					}
				}
			}
		})
	}
	for _, door := range []string{"create", "update"} {
		t.Run("link refusal/"+door, func(t *testing.T) {
			f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{}, graphHard())
			f.link(1, 2, "target", "deadline", "")
			if door == "update" {
				f.link(0, 1, "target", "target", "")
			}
			g := f.guest(0, 1)
			graphPushDevice(t, f, f.owner)
			body := map[string]any{"offset": map[string]any{"mode": "elapsed", "elapsed_secs": 20 * 86400}, "reason": "Replan"}
			method, path := "POST", f.path(f.item)+"/links"
			if door == "create" {
				body["child"] = f.items[1].ID
				body["parent_role"] = "target"
				body["child_role"] = "target"
			} else {
				var id string
				if err := f.srv.store.DB().QueryRow(`SELECT l.id FROM schedule_links l JOIN item_schedules sc ON sc.id=l.parent_schedule_id WHERE sc.item_id=?`, f.item.ID).Scan(&id); err != nil {
					t.Fatal(err)
				}
				method, path = "PATCH", f.resourcePath("links", id)
			}
			var refusal string
			for i := 0; i < 2; i++ {
				rr := f.as(f.token(g, ""), method, path, body)
				f.expectErr(rr, 409, "deadline_approval_required", "")
				if i == 0 {
					refusal = rr.Body.String()
				} else if rr.Body.String() != refusal {
					t.Fatal("link dedupe changed refusal bytes")
				}
			}
			if n := f.count(`SELECT COUNT(*) FROM user_notifications WHERE kind='deadline_approval_requested' AND user_id=?`, f.owner.ID); n != 1 {
				t.Fatalf("link approval notices=%d", n)
			}
			if _, err := f.srv.store.DB().Exec(`DELETE FROM push_devices`); err != nil {
				t.Fatal(err)
			}
			if rr := f.as(f.token(g, ""), method, path, body); rr.Code != 409 || rr.Body.String() != refusal {
				t.Fatal("link no-recipient changed refusal bytes")
			}
			f.srv.enqueueCommitmentLinkRefusal = func(string, *models.ScheduleLinkCreate, string, *models.ScheduleLinkUpdate) error {
				return errors.New("private-link-notice-error")
			}
			if rr := f.as(f.token(g, ""), method, path, body); rr.Code != 409 || rr.Body.String() != refusal {
				t.Fatal("link enqueue failure changed refusal bytes")
			}
		})
	}
}
