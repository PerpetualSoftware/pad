package store

import (
	"database/sql"
	"encoding/json"
	"github.com/PerpetualSoftware/pad/internal/models"
	"strings"
	"testing"
	"time"
)

func graphCommittedNoticeTest(t *testing.T) {
	f := newLinkFix(t, 3)
	recipient := notifyUser(t, f.s, "graph-recipient@example.com")
	if err := f.s.AddWorkspaceMember(f.ws.ID, recipient.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	addDevice(t, f.s, recipient.ID)
	for _, it := range f.items {
		f.exec(t, `UPDATE items SET assigned_user_id=? WHERE id=?`, recipient.ID, it.ID)
	}
	f.linkRaw(t, 0, 1, "target", "target", oneDay)
	f.linkRaw(t, 0, 2, "target", "target", oneDay)
	if _, err := f.move(0, "target", "2030-04-01T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='commitment_moved' AND user_id=?`, recipient.ID); n != 1 {
		t.Fatalf("move notices=%d", n)
	}
}
func graphHistoryMoveTest(t *testing.T) {
	f := newLinkFix(t, 2)
	f.linkRaw(t, 0, 1, "target", "target", oneDay)
	if _, err := f.move(0, "target", "2030-04-01T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	c, err := f.s.GetCommitmentFor(f.ws.ID, f.items[1].ID, func(id string) bool { return id == f.items[1].ID })
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Journal) == 0 {
		t.Fatal("dependent move missing from History")
	}
	for _, ch := range c.Journal {
		if ch.RootChangeID != "" || ch.ParentChangeID != nil {
			t.Fatal("hidden lineage exposed")
		}
	}
}
func TestTASK4423_R11SkipAuthority(t *testing.T) {
	t.Run("parent result is identical", func(t *testing.T) {
		world := func(linked bool) string {
			f := newLinkFix(t, 2)
			f.deadline(t, 1, "stays_owed")
			if linked {
				f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
			}
			res, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, ResolveInput{OccurrenceAt: rt(*f.reload(t, f.items[0].ID).NextAt), State: "skip", Actor: "user", MaySkipBeforeDeadline: true, LinkAccess: f.access(0)}, rt("2029-01-01T00:00:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			res.Schedule.ID, res.Schedule.ItemID, res.Schedule.WorkspaceID, res.Schedule.CreatedAt = "", "", "", ""
			b, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		if with, without := world(true), world(false); with != without {
			t.Fatalf("child changed parent result:\n%s\n%s", with, without)
		}
	})

	for _, mode := range []string{"parent-only", "child-edit", "no-deadline", "nil", "legacy-deadline", "broken-deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := newLinkFix(t, 2)
			f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
			if mode != "no-deadline" {
				f.deadline(t, 1, "stays_owed")
			}
			if mode == "legacy-deadline" {
				f.exec(t, `UPDATE item_schedules SET roles_origin='legacy_backfill',kind='deadline_hard' WHERE id=?`, f.scheds[1].ID)
			}
			if mode == "broken-deadline" {
				f.exec(t, `UPDATE item_schedules SET error_code='unsupported' WHERE id=?`, f.scheds[1].ID)
			}
			reminderBefore := renegotiateRows(t, f.s, "item_reminders", ` WHERE schedule_id=?`, f.scheds[1].ID)
			acc := f.access(0)
			if mode == "child-edit" {
				acc = f.allEdit()
			}
			in := ResolveInput{OccurrenceAt: rt(*f.reload(t, f.items[0].ID).NextAt), State: "skip", Actor: "user", MaySkipBeforeDeadline: true}
			if mode != "nil" {
				in.LinkAccess = acc
			}
			if _, err := f.s.ResolveOccurrence(f.ws.ID, f.items[0].ID, in); err != nil {
				t.Fatal(err)
			}
			want := mode == "child-edit" || mode == "no-deadline"
			if got := f.noLongerNeeded(t, 1); got != want {
				t.Fatalf("marked=%v want %v", got, want)
			}
			if !want {
				if after := renegotiateRows(t, f.s, "item_reminders", ` WHERE schedule_id=?`, f.scheds[1].ID); after != reminderBefore {
					t.Fatal("protected child reminder changed")
				}
				if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND reason='No longer needed'`, f.scheds[1].ID); n != 0 {
					t.Fatal("protected child journal changed")
				}
			}
			if !want && mode != "legacy-deadline" && mode != "broken-deadline" {
				f.eval(t, farFuture, 0)
				if f.misses(t, 1) != 1 {
					t.Fatal("protected deadline did not record miss")
				}
			}
		})
	}
}

func graphRefusedNoticeTest(t *testing.T) {
	f := newLinkFix(t, 2)
	f.deadline(t, 1, "stays_owed")
	f.linkRaw(t, 0, 1, "target", "deadline", oneDay)
	recipient := notifyUser(t, f.s, "approval-recipient@example.com")
	actor := notifyUser(t, f.s, "approval-actor@example.com")
	for _, u := range []*models.User{recipient, actor} {
		if err := f.s.AddWorkspaceMember(f.ws.ID, u.ID, "viewer"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.CreateItemGrant(f.ws.ID, f.items[0].ID, actor.ID, "edit", recipient.ID); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE items SET assigned_user_id=? WHERE id=?`, recipient.ID, f.items[1].ID)
	addDevice(t, f.s, recipient.ID)
	in := models.LinkedMoveInput{ItemID: f.items[0].ID, Role: "target", To: "2030-04-01T09:00:00Z", Actor: actor.ID, Access: f.access(0), Reason: "private reason"}
	before := renegotiateRows(t, f.s, "item_schedules", "") + renegotiateRows(t, f.s, "schedule_changes", "")
	if _, err := f.s.MoveLinkedDate(f.ws.ID, in); err != ErrDeadlineApprovalRequired {
		t.Fatalf("refusal=%v", err)
	}
	if after := renegotiateRows(t, f.s, "item_schedules", "") + renegotiateRows(t, f.s, "schedule_changes", ""); after != before {
		t.Fatal("refusal changed dates or journal")
	}
	until := rt(in.To)
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, in.ItemID, ResolveInput{OccurrenceAt: rt(*f.reload(t, in.ItemID).NextAt), State: "move", Actor: actor.ID, Until: &until, LinkAccess: in.Access, Comment: f.comment(f.items[0], actor.ID, "Private comment")}, rt("2030-03-01T00:00:00Z")); err != ErrDeadlineApprovalRequired {
		t.Fatalf("resolve refusal=%v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM comments`); n != 0 {
		t.Fatal("refusal committed a comment")
	}
	for i := 0; i < 2; i++ {
		in.Revision = "ignored-client-spelling"
		if err := f.s.EnqueueLinkedDeadlineApprovalNotice(f.ws.ID, in); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='deadline_approval_requested'`); n != 1 {
		t.Fatalf("repeated notice=%d", n)
	}
	in.To = "2030-04-02T09:00:00Z"
	if err := f.s.EnqueueLinkedDeadlineApprovalNotice(f.ws.ID, in); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='deadline_approval_requested'`); n != 2 {
		t.Fatalf("different operation notices=%d", n)
	}
	f.exec(t, `UPDATE schedule_links SET offset_elapsed=offset_elapsed+3600 WHERE workspace_id=?`, f.ws.ID)
	if err := f.s.EnqueueLinkedDeadlineApprovalNotice(f.ws.ID, in); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='deadline_approval_requested'`); n != 3 {
		t.Fatalf("changed graph notices=%d", n)
	}
	if _, err := f.s.CreateItemGrant(f.ws.ID, f.items[1].ID, actor.ID, "edit", recipient.ID); err != nil {
		t.Fatal(err)
	}
	in.To = "2030-04-03T09:00:00Z"
	if err := f.s.EnqueueLinkedDeadlineApprovalNotice(f.ws.ID, in); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='deadline_approval_requested'`); n != 3 {
		t.Fatalf("stale refusal notices=%d", n)
	}
	// The journal actor can be a kind. The person is what the refusal is
	// recomputed for.
	if _, err := f.s.db.Exec(f.s.q(`DELETE FROM item_grants WHERE item_id=? AND user_id=?`), f.items[1].ID, actor.ID); err != nil {
		t.Fatal(err)
	}
	in.Actor, in.ActorUserID, in.To = "user", actor.ID, "2030-04-04T09:00:00Z"
	if err := f.s.EnqueueLinkedDeadlineApprovalNotice(f.ws.ID, in); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='deadline_approval_requested'`); n != 4 {
		t.Fatalf("notice by user id=%d", n)
	}
}

func TestTASK4423_R05Notices(t *testing.T) {
	t.Run("committed", graphCommittedNoticeTest)
	t.Run("refused", graphRefusedNoticeTest)
	t.Run("recipients", graphNoticeRecipientTests)
}

func TestTASK4423_R05History(t *testing.T) {
	t.Run("target", graphHistoryMoveTest)
	for _, role := range []string{"target", "earliest", "occupied_until"} {
		t.Run(role+" dates and conflicts", func(t *testing.T) {
			f := newLinkFix(t, 2)
			f.deadline(t, 1, "stays_owed")
			f.exec(t, `UPDATE item_schedules SET earliest_days=0,earliest_secs=0,occupied_days=0,occupied_secs=3600 WHERE id=?`, f.scheds[1].ID)
			f.linkRaw(t, 0, 1, "target", role, oneDay)
			if _, err := f.move(0, "target", "2030-04-01T09:00:00Z", func(in *models.LinkedMoveInput) {
				in.Access = f.access(0)
				delete(in.Access.Readable, f.items[1].ID)
				in.Reason = "Private parent text"
			}); err != nil {
				t.Fatal(err)
			}
			if role == "earliest" {
				f.exec(t, `UPDATE schedule_changes SET reason='Private parent text',reason_source='user' WHERE item_id=? AND change_type='move'`, f.items[1].ID)
			}
			c, err := f.s.GetCommitmentFor(f.ws.ID, f.items[1].ID, func(id string) bool { return id == f.items[1].ID })
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Journal) != 1 || c.Journal[0].Role == nil || *c.Journal[0].Role != role || c.Journal[0].BeforeJSON == nil || c.Journal[0].AfterJSON == nil {
				t.Fatalf("History=%+v", c.Journal)
			}
			if role == "target" && len(c.Conflicts) < 1 {
				t.Fatal("own-date conflict missing")
			}
			b, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), f.items[0].ID) || strings.Contains(string(b), "Private parent text") || c.Journal[0].ParentChangeID != nil || c.Journal[0].RootChangeID != "" {
				t.Fatalf("cross-item information: %s", b)
			}
		})
	}
}

func graphNoticeRecipientTests(t *testing.T) {
	for _, mode := range []string{"assignee", "owner", "no-access", "no-device", "self", "self-by-user-id", "agent-for-user", "self-resolve", "self-resolve-no-comment", "preview", "rollback", "past-deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := newLinkFix(t, 2)
			recipient := notifyUser(t, f.s, "move-recipient@example.com")
			if err := f.s.AddWorkspaceMember(f.ws.ID, recipient.ID, "viewer"); err != nil {
				t.Fatal(err)
			}
			if mode == "owner" {
				f.exec(t, `UPDATE workspaces SET owner_id=? WHERE id=?`, recipient.ID, f.ws.ID)
			} else {
				f.exec(t, `UPDATE items SET assigned_user_id=? WHERE id=?`, recipient.ID, f.items[1].ID)
			}
			if mode != "no-device" {
				addDevice(t, f.s, recipient.ID)
			}
			if mode == "no-access" {
				if err := f.s.SetMemberCollectionAccess(f.ws.ID, recipient.ID, "specific", []string{}); err != nil {
					t.Fatal(err)
				}
			}
			f.linkRaw(t, 0, 1, "target", "target", oneDay)
			if mode == "past-deadline" {
				f.deadline(t, 1, "stays_owed")
			}
			if mode == "rollback" {
				clock := rt("2029-01-01T00:00:00Z")
				f.s.linkClock = func() time.Time { return clock }
				f.s.beforeLinkCommit = func() { clock = clock.Add(linkTxBudget + time.Second) }
			}
			var err error
			if mode == "self-resolve" || mode == "self-resolve-no-comment" {
				until := rt("2030-04-01T09:00:00Z")
				in := ResolveInput{OccurrenceAt: rt(*f.reload(t, f.items[0].ID).NextAt), State: "move", Until: &until, Actor: "user", LinkAccess: f.allEdit()}
				if mode == "self-resolve" {
					in.Comment = f.comment(f.items[0], recipient.ID, "Reschedule")
				} else {
					in.ActorUserID = recipient.ID
				}
				_, err = f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, in, rt("2030-03-01T00:00:00Z"))
			} else {
				_, err = f.move(0, "target", "2030-04-01T09:00:00Z", func(in *models.LinkedMoveInput) {
					if mode == "self" {
						in.Actor = recipient.ID
					}
					if mode == "self-by-user-id" {
						in.Actor, in.ActorUserID = "user", recipient.ID
					}
					if mode == "agent-for-user" {
						in.Actor, in.ActorUserID = "agent", recipient.ID
					}
					in.Preview = mode == "preview"
					if mode == "past-deadline" {
						delete(in.Access.Readable, f.items[1].ID)
					}
				})
			}
			if mode == "rollback" {
				if err == nil {
					t.Fatal("rollback fault did not refuse")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "assignee" || mode == "owner" || mode == "past-deadline" || mode == "agent-for-user" {
				want = 1
			}
			if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind IN ('commitment_moved','commitment_moved_past_deadline')`); n != want {
				t.Fatalf("notices=%d want %d", n, want)
			}
			if want == 1 {
				var subject, kind string
				var comment, actor sql.NullString
				if err := f.s.db.QueryRow(f.s.q(`SELECT item_id,kind,comment_id,actor_user_id FROM user_notifications WHERE kind IN ('commitment_moved','commitment_moved_past_deadline')`)).Scan(&subject, &kind, &comment, &actor); err != nil {
					t.Fatal(err)
				}
				if mode != "owner" && subject != f.items[1].ID || comment.Valid || actor.Valid {
					t.Fatal("notice carries information beyond its subject")
				}
				if mode == "past-deadline" && kind != "commitment_moved_past_deadline" {
					t.Fatalf("kind=%s", kind)
				}
			}
		})
	}
}
