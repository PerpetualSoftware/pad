package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func TestCommitmentNotices_Settings(t *testing.T) {
	for _, kind := range []string{"unassigned", "renegotiation_refused"} {
		set := models.DefaultNotificationSettings()
		n := store.Notification{Kind: kind}
		if reason := kindEnabled(set, n); reason != "" {
			t.Errorf("%s enabled: %s", kind, reason)
		}
		set.NotifyAssigned = false
		if reason := kindEnabled(set, n); reason != skipSettingOff {
			t.Errorf("%s disabled: %s", kind, reason)
		}
	}
}

func TestCommitmentNotices_DispatchAndItemGates(t *testing.T) {
	for _, kind := range []string{"unassigned", "renegotiation_refused"} {
		for _, assigned := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/away", true: "/assigned"}[assigned], func(t *testing.T) {
				f := newPdx(t, testServer(t))
				if !assigned {
					f.exec(`UPDATE items SET assigned_user_id=NULL WHERE id=?`, f.item.ID)
				}
				id := f.row(kind)
				f.tick()
				shouldSend := (kind == "unassigned" && !assigned) || (kind == "renegotiation_refused" && assigned)
				state := f.state(id)
				if !shouldSend {
					reason := "assigned_again"
					if kind == "renegotiation_refused" {
						reason = skipNoLongerAssign
					}
					if state.State != "skipped" || state.Reason != reason || len(f.fake.all()) != 0 {
						t.Fatalf("state=%+v messages=%v", state, f.fake.all())
					}
					return
				}
				messages := f.fake.all()
				want := "You were unassigned from an item"
				if kind == "renegotiation_refused" {
					want = "A deadline change on your item was refused"
				}
				if state.State != "sent" || len(messages) != 1 || messages[0].Body != want || messages[0].Data.Kind != kind {
					t.Fatalf("state=%+v messages=%v", state, messages)
				}
				if _, ok := kindPriority[kind]; !ok {
					t.Fatal("missing kind priority")
				}
			})
		}
	}
}

func TestCommitmentNotices_RefusalResponseUnchanged(t *testing.T) {
	f := newSchedHTTP(t)
	editor := f.member("refused-editor@example.com", "editor")
	viewer := f.member("refused-viewer@example.com", "viewer")
	readCommitment(t, commitmentCall(f, "POST", commitmentBody()), 201)
	if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &f.owner.ID, ActorUserID: f.owner.ID}); err != nil {
		t.Fatal(err)
	}
	// The existing helper records the exact response before enqueue is wired.
	before := httptest.NewRecorder()
	writeDeadlineOwnerRequired(before)
	path := commitmentPath(f) + "/renegotiate"
	tok := f.token(editor, "")
	rr := f.as(tok, "POST", path, `bad json`)
	if rr.Code != before.Code || rr.Body.String() != before.Body.String() {
		t.Fatalf("response changed: %d %s", rr.Code, rr.Body.String())
	}
	// Device registration is the only difference for the second attempt.
	plain, err := f.srv.store.CreateSession(f.owner.ID, "ios", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.srv.store.ValidateSession(plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.RegisterPushDevice(f.owner.ID, session.ID, "refusal-device", "ios", "refusal-token", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", path, strings.NewReader(`bad json`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Pad-Agent", "test-agent")
		rr = httptest.NewRecorder()
		f.srv.ServeHTTP(rr, req)
		if rr.Code != before.Code || rr.Body.String() != before.Body.String() {
			t.Fatalf("response changed: %d %s", rr.Code, rr.Body.String())
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM user_notifications WHERE kind='renegotiation_refused' AND user_id=? AND actor_user_id=? AND actor_is_agent=1`, f.owner.ID, editor.ID); n != 1 {
		t.Fatalf("refusal rows=%d", n)
	}
	if rr := f.as(f.token(viewer, ""), "POST", path, `bad json`); rr.Code != 403 {
		t.Fatalf("viewer: %d %s", rr.Code, rr.Body.String())
	}
	if rr := f.as(tok, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/items/missing/commitment/renegotiate", `bad json`); rr.Code != 404 {
		t.Fatalf("missing item: %d %s", rr.Code, rr.Body.String())
	}
	if n := f.count(`SELECT COUNT(*) FROM user_notifications`); n != 1 {
		t.Fatalf("early refusal enqueued: %d", n)
	}
	if _, err := f.srv.store.DB().Exec(`CREATE TRIGGER refuse_notice_insert BEFORE INSERT ON user_notifications BEGIN SELECT RAISE(ABORT, 'enqueue unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if rr := f.as(tok, "POST", path, `bad json`); rr.Code != before.Code || rr.Body.String() != before.Body.String() {
		t.Fatalf("enqueue error changed refusal: %d %s", rr.Code, rr.Body.String())
	}
	if f.comments() != 0 {
		t.Fatal("refusal wrote a comment")
	}
	if n := f.count(`SELECT COUNT(*) FROM watches`); n != 0 {
		t.Fatal("refusal subscribed")
	}
}
