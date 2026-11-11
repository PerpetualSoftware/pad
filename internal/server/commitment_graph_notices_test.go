package server

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func TestTASK4423_R05Notices(t *testing.T) {
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
