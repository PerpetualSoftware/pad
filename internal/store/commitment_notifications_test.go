package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func refusalNotice(t *testing.T, s *Store, ws, item, actor string, agent bool) {
	t.Helper()
	if err := s.EnqueueRenegotiationRefusedNotification(ws, item, actor, agent); err != nil {
		t.Fatal(err)
	}
}

func noticeFixture(t *testing.T, native bool) (notifyFixture, *models.User, *models.User) {
	t.Helper()
	f := newNotifyFixture(t)
	recipient, actor := notifyUser(t, f.s, "recipient@example.com"), notifyUser(t, f.s, "actor@example.com")
	f.join(t, recipient, actor)
	f.s.mustExec(t, `UPDATE items SET assigned_user_id=? WHERE id=?`, recipient.ID, f.item.ID)
	if native {
		if _, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Tz: "UTC"}, nativeRoles()); err != nil {
			t.Fatal(err)
		}
	}
	return f, recipient, actor
}

func TestCommitmentNotices_ProvenanceKeysAndNoSubscriptions(t *testing.T) {
	for _, kind := range []string{"unassigned", "renegotiation_refused"} {
		for _, writer := range []string{"human", "agent", "system"} {
			t.Run(kind+"/"+writer, func(t *testing.T) {
				f, recipient, actor := noticeFixture(t, true)
				addDevice(t, f.s, recipient.ID)
				actorID := actor.ID
				if writer == "system" {
					actorID = ""
				}
				if kind == "unassigned" {
					by := "user"
					if writer == "agent" {
						by = "agent"
					}
					if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &actor.ID, ActorUserID: actorID, LastModifiedBy: by}); err != nil {
						t.Fatal(err)
					}
				} else {
					refusalNotice(t, f.s, f.ws.ID, f.item.ID, actorID, writer == "agent")
					refusalNotice(t, f.s, f.ws.ID, f.item.ID, actorID, writer == "agent")
				}
				var user, key string
				var gotActor, comment sql.NullString
				var agent bool
				err := f.s.db.QueryRow(f.s.q(`SELECT user_id,actor_user_id,actor_is_agent,comment_id,dedupe_key FROM user_notifications WHERE kind=?`), kind).Scan(&user, &gotActor, &agent, &comment, &key)
				if err != nil {
					t.Fatal(err)
				}
				if user != recipient.ID || gotActor.String != actorID || gotActor.Valid != (actorID != "") || agent != (writer == "agent") || comment.Valid {
					t.Fatalf("incorrect provenance: %s %v %v %v", user, gotActor, agent, comment)
				}
				if kind == "unassigned" {
					var journalID string
					if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_changes WHERE item_id=? AND reason=?`), f.item.ID, "Assignee changed from "+recipient.ID+" to "+actor.ID).Scan(&journalID); err != nil {
						t.Fatal(err)
					}
					if key != "unassigned:"+journalID {
						t.Fatalf("key=%s journal=%s", key, journalID)
					}
				} else if key != "reneg-refused:"+f.item.ID+":"+actorID+":"+time.Now().UTC().Format("2006-01-02") {
					t.Fatalf("key=%s", key)
				}
				if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE kind=?`, kind); n != 1 {
					t.Fatalf("rows=%d", n)
				}
				for _, table := range []string{"comments", "watches"} {
					if n := f.s.countRows(t, `SELECT COUNT(*) FROM `+table); n != 0 {
						t.Fatalf("unexpected %s: %d", table, n)
					}
				}
			})
		}
	}
}

func TestCommitmentNotices_RecipientGates(t *testing.T) {
	for _, kind := range []string{"unassigned", "renegotiation_refused"} {
		for _, gate := range []string{"self", "guest_without_grant", "removed_member", "removed_admin", "restricted", "no_device", "collection_access", "item_grant"} {
			t.Run(kind+"/"+gate, func(t *testing.T) {
				f, recipient, actor := noticeFixture(t, true)
				if gate != "no_device" {
					addDevice(t, f.s, recipient.ID)
				}
				if gate == "self" {
					actor = recipient
				}
				switch gate {
				case "guest_without_grant":
					f.s.mustExec(t, `DELETE FROM workspace_members WHERE workspace_id=? AND user_id=?`, f.ws.ID, recipient.ID)
				case "removed_member", "removed_admin":
					if err := f.s.RemoveWorkspaceMember(f.ws.ID, recipient.ID); err != nil {
						t.Fatal(err)
					}
					if gate == "removed_admin" {
						f.s.mustExec(t, `UPDATE users SET role='admin' WHERE id=?`, recipient.ID)
					}
				case "collection_access":
					if err := f.s.SetMemberCollectionAccess(f.ws.ID, recipient.ID, "specific", []string{f.coll.ID}); err != nil {
						t.Fatal(err)
					}
				case "item_grant":
					if err := f.s.SetMemberCollectionAccess(f.ws.ID, recipient.ID, "specific", []string{}); err != nil {
						t.Fatal(err)
					}
					if _, err := f.s.CreateItemGrant(f.ws.ID, f.item.ID, recipient.ID, "read", actor.ID); err != nil {
						t.Fatal(err)
					}
				case "restricted":
					if err := f.s.SetMemberCollectionAccess(f.ws.ID, recipient.ID, "specific", []string{}); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "unassigned" {
					empty := ""
					if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &empty, ActorUserID: actor.ID}); err != nil {
						t.Fatal(err)
					}
				} else {
					refusalNotice(t, f.s, f.ws.ID, f.item.ID, actor.ID, false)
				}
				want := 0
				if gate == "collection_access" || gate == "item_grant" {
					want = 1
				}
				if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE kind=?`, kind); n != want {
					t.Fatalf("rows=%d", n)
				}
			})
		}
	}
}

func TestCommitmentNotices_UnassignedWithoutScheduleAndEachChange(t *testing.T) {
	f, recipient, actor := noticeFixture(t, false)
	addDevice(t, f.s, recipient.ID)
	// The unchanged-assignee path must not enqueue. There is no statement-count hook.
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &recipient.ID, ActorUserID: actor.ID}); err != nil {
		t.Fatal(err)
	}
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications`); n != 0 {
		t.Fatalf("unchanged rows=%d", n)
	}
	for i := 0; i < 2; i++ {
		empty := ""
		updated, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &empty, ActorUserID: actor.ID})
		if err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("unassigned:%s:%d", f.item.ID, updated.Seq)
		if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id=? AND kind='unassigned' AND dedupe_key=?`, recipient.ID, key); n != 1 {
			t.Fatalf("change %d rows=%d", i, n)
		}
		if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &recipient.ID, ActorUserID: actor.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='unassigned'`); n != 2 {
		t.Fatalf("rows=%d", n)
	}
}

func TestCommitmentNotices_RefusalReadsCurrentAssignee(t *testing.T) {
	f, previous, actor := noticeFixture(t, true)
	addDevice(t, f.s, previous.ID)
	addDevice(t, f.s, actor.ID)
	f.s.mustExec(t, `UPDATE items SET assigned_user_id=? WHERE id=?`, actor.ID, f.item.ID)
	refusalNotice(t, f.s, f.ws.ID, f.item.ID, previous.ID, false)
	wantKinds(t, "current assignee", f.kinds(t, actor.ID), "renegotiation_refused")
	wantKinds(t, "previous assignee", f.kinds(t, previous.ID))
	f.s.mustExec(t, `UPDATE items SET assigned_user_id=NULL WHERE id=?`, f.item.ID)
	refusalNotice(t, f.s, f.ws.ID, f.item.ID, previous.ID, false)
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications`); n != 1 {
		t.Fatalf("rows=%d", n)
	}
}
