package server

import (
	"net/http"
	"testing"

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
