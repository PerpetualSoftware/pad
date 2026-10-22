package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3939 part B1: the dispatcher IS the privacy gate. Every case seeds a
// pending row for a recipient who has a device, breaks ONE thing, ticks, and
// asserts the row is skipped with that reason and the gateway was never
// called; every skip case has a control that sends, so a skip is not just a
// misconfigured fixture. Each gate has a mutant in the report that turns its
// case red.

type gateCase struct {
	name   string
	kind   string // default comment
	setup  func(f *pdx, id string)
	row    []func(*rowOpt)
	reason string                  // "" = the row is pushed
	check  func(f *pdx, id string) // extra assertions on a pushed row
}

func gateCases() []gateCase {
	removeBob := func(f *pdx, _ string) { must(f.t, f.srv.store.RemoveWorkspaceMember(f.ws.ID, f.bob.ID)) }
	specificNone := func(f *pdx, _ string) {
		must(f.t, f.srv.store.SetMemberCollectionAccess(f.ws.ID, f.bob.ID, "specific", []string{}))
	}
	offKind := func(mut func(*models.NotificationSettings)) func(*pdx, string) {
		return func(f *pdx, _ string) { f.settings(mut) }
	}
	otherWorkspace := func(f *pdx) *models.Workspace {
		ws2, err := f.srv.store.CreateWorkspace(models.WorkspaceCreate{Name: fmt.Sprintf("Other %d", f.n), OwnerID: f.owner.ID})
		must(f.t, err)
		must(f.t, f.srv.store.AddWorkspaceMember(ws2.ID, f.bob.ID, "editor"))
		return ws2
	}
	return []gateCase{
		// controls: each kind, as the fixture leaves it
		{name: "control_comment", kind: "comment"},
		{name: "control_reply", kind: "reply"},
		{name: "control_assigned", kind: "assigned"},
		{name: "control_reminder", kind: "reminder"},
		{name: "control_mention_default_settings", kind: "mention"},

		// G1 membership, separate from visibility
		{name: "removed_member_still_assignee", kind: "assigned", setup: removeBob, reason: skipNotMember},
		{name: "removed_member_still_creator_comment", kind: "comment", setup: removeBob, reason: skipNotMember},
		{name: "platform_admin_not_member", kind: "comment", reason: skipNotMember, setup: func(f *pdx, id string) {
			f.exec(`UPDATE users SET role = 'admin' WHERE id = ?`, f.bob.ID)
			removeBob(f, id)
		}},

		// G2 visibility
		{name: "specific_access_without_the_collection", kind: "comment", setup: specificNone, reason: skipNotVisible},
		{name: "specific_access_with_the_collection", kind: "comment", setup: func(f *pdx, _ string) {
			must(f.t, f.srv.store.SetMemberCollectionAccess(f.ws.ID, f.bob.ID, "specific", []string{f.coll.ID}))
		}},
		{name: "admin_member_restricted_is_not_bypassed", kind: "comment", reason: skipNotVisible, setup: func(f *pdx, id string) {
			f.exec(`UPDATE users SET role = 'admin' WHERE id = ?`, f.bob.ID)
			specificNone(f, id)
		}},

		// G3 liveness of the subject
		{name: "item_soft_deleted", kind: "assigned", reason: skipItemGone, setup: func(f *pdx, _ string) {
			must(f.t, f.srv.store.DeleteItem(f.item.ID))
		}},
		{name: "collection_archived", kind: "assigned", reason: skipItemGone, setup: func(f *pdx, _ string) {
			f.exec(`UPDATE collections SET deleted_at = ? WHERE id = ?`, f.ts(time.Minute), f.coll.ID)
		}},
		// user_notifications.item_id has no foreign key: the row outlives a purged item.
		{name: "item_hard_deleted", kind: "reminder", reason: skipItemGone, setup: func(f *pdx, id string) {
			f.exec(`UPDATE user_notifications SET item_id = 'purged-item' WHERE id = ?`, id)
		}},
		{name: "workspace_soft_deleted", kind: "assigned", reason: skipWorkspaceGone, setup: func(f *pdx, _ string) {
			must(f.t, f.srv.store.DeleteWorkspace(f.ws.Slug))
		}},
		{name: "row_workspace_is_not_the_items", kind: "assigned", reason: skipItemGone, setup: func(f *pdx, id string) {
			f.exec(`UPDATE user_notifications SET workspace_id = ? WHERE id = ?`, otherWorkspace(f).ID, id)
		}},

		// G4 the user
		{name: "user_disabled", kind: "comment", reason: skipUserDisabled, setup: func(f *pdx, _ string) {
			f.exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, f.ts(time.Minute), f.bob.ID)
		}},
		{name: "app_principal", kind: "comment", reason: skipUserDisabled, setup: func(f *pdx, _ string) {
			f.exec(`UPDATE users SET kind = 'app' WHERE id = ?`, f.bob.ID)
		}},

		// G9 the reason for the row still holds
		{name: "assigned_away", kind: "assigned", reason: skipNoLongerAssign, setup: func(f *pdx, _ string) {
			f.exec(`UPDATE items SET assigned_user_id = ? WHERE id = ?`, f.owner.ID, f.item.ID)
		}},
		{name: "unassigned", kind: "assigned", reason: skipNoLongerAssign, setup: func(f *pdx, _ string) {
			f.exec(`UPDATE items SET assigned_user_id = NULL WHERE id = ?`, f.item.ID)
		}},
		{name: "assigned_control", kind: "assigned", setup: func(f *pdx, _ string) {
			f.exec(`UPDATE items SET assigned_user_id = ? WHERE id = ?`, f.owner.ID, f.item.ID)
			f.exec(`UPDATE items SET assigned_user_id = ? WHERE id = ?`, f.bob.ID, f.item.ID)
		}},
		{name: "comment_tombstoned", kind: "comment", reason: skipCommentGone, setup: func(f *pdx, _ string) {
			f.exec(`UPDATE comments SET deleted_at = ? WHERE id = ?`, f.ts(time.Minute), f.comment.ID)
		}},
		{name: "reply_comment_hard_deleted", kind: "reply", reason: skipCommentGone, setup: func(f *pdx, _ string) {
			f.exec(`DELETE FROM comments WHERE id = ?`, f.comment.ID)
		}},
		{name: "mention_comment_hard_deleted", kind: "mention", reason: skipCommentGone, setup: func(f *pdx, _ string) {
			f.exec(`DELETE FROM comments WHERE id = ?`, f.comment.ID)
		}},
		{name: "comment_row_without_a_comment_id", kind: "comment", reason: skipCommentGone, row: []func(*rowOpt){func(o *rowOpt) { o.comment = "" }}},

		// the row's comment must be a comment on the row's item, in its workspace
		{name: "comment_belongs_to_another_item", kind: "comment", reason: skipCommentGone, setup: func(f *pdx, id string) {
			other := f.newItem("Other", &f.bob.ID)
			cm, err := f.srv.store.CreateComment(f.ws.ID, other.ID, f.owner.ID, models.CommentCreate{Body: "x", Author: "Owner", CreatedBy: "user", Source: "web"})
			must(f.t, err)
			f.exec(`UPDATE user_notifications SET comment_id = ? WHERE id = ?`, cm.ID, id)
		}},
		{name: "comment_belongs_to_another_workspace", kind: "comment", reason: skipCommentGone, setup: func(f *pdx, _ string) {
			f.exec(`UPDATE comments SET workspace_id = ? WHERE id = ?`, otherWorkspace(f).ID, f.comment.ID)
		}},

		// G5 settings
		{name: "push_disabled", kind: "comment", reason: skipSettingOff, setup: offKind(func(s *models.NotificationSettings) { s.PushEnabled = false })},
		{name: "assigned_off", kind: "assigned", reason: skipSettingOff, setup: offKind(func(s *models.NotificationSettings) { s.NotifyAssigned = false })},
		{name: "comments_off", kind: "comment", reason: skipSettingOff, setup: offKind(func(s *models.NotificationSettings) { s.NotifyComments = false })},
		{name: "replies_off", kind: "reply", reason: skipSettingOff, setup: offKind(func(s *models.NotificationSettings) { s.NotifyReplies = false })},
		{name: "reminders_off", kind: "reminder", reason: skipSettingOff, setup: offKind(func(s *models.NotificationSettings) { s.NotifyReminders = false })},
		{name: "mentions_off", kind: "mention", reason: skipSettingOff, setup: offKind(func(s *models.NotificationSettings) { s.NotifyMentions = false })},
		// comment and reply are separate switches: each off leaves the other on
		{name: "comments_off_reply_still_sends", kind: "reply", setup: offKind(func(s *models.NotificationSettings) { s.NotifyComments = false })},
		{name: "replies_off_comment_still_sends", kind: "comment", setup: offKind(func(s *models.NotificationSettings) { s.NotifyReplies = false })},
		{name: "mentions_off_comment_still_sends", kind: "comment", setup: offKind(func(s *models.NotificationSettings) { s.NotifyMentions = false })},

		// G6 agent activity
		{name: "agent_actor_default_off", kind: "comment", reason: skipAgentOff, row: []func(*rowOpt){func(o *rowOpt) { o.agent = true }}},
		{name: "agent_actor_opted_in", kind: "comment", row: []func(*rowOpt){func(o *rowOpt) { o.agent = true }},
			setup: offKind(func(s *models.NotificationSettings) { s.IncludeAgentActivity = true })},

		// G7 devices
		{name: "no_device", kind: "comment", reason: skipNoDevice, setup: func(f *pdx, _ string) {
			f.exec(`DELETE FROM push_devices WHERE user_id = ?`, f.bob.ID)
		}},
		{name: "row_predates_the_only_device", kind: "comment", reason: skipPredatesDevice, setup: func(f *pdx, _ string) {
			f.exec(`UPDATE push_devices SET created_at = ? WHERE id = ?`, f.ts(30*time.Second), f.devID)
		}},
		{name: "claim_race_devices_deleted_then_new_device_registers", kind: "assigned", reason: skipPredatesDevice, setup: func(f *pdx, _ string) {
			f.exec(`DELETE FROM push_devices WHERE user_id = ?`, f.bob.ID)
			f.device(f.bob, "tok-next-holder", 30*time.Second)
		}},
		{name: "row_after_the_device_control", kind: "assigned", setup: func(f *pdx, _ string) {
			f.exec(`UPDATE push_devices SET created_at = ? WHERE id = ?`, f.ts(time.Hour), f.devID)
		}},
		{name: "second_device_after_the_row_older_device_live", kind: "comment", setup: func(f *pdx, _ string) {
			f.device(f.bob, "tok-second", 30*time.Second)
		}, check: func(f *pdx, _ string) {
			devs, err := f.srv.store.LivePushDevices(f.bob.ID, time.Now())
			must(f.t, err)
			if len(devs) != 2 || devs[0].ID != f.devID {
				f.t.Fatalf("live devices = %+v, want the earliest (%s) first of two", devs, f.devID)
			}
			got := map[string]int{}
			for _, m := range f.fake.all() {
				got[m.Token]++
			}
			if len(got) != 2 || got["tok-bob-"+fmt.Sprint(f.n)] != 1 || got["tok-second"] != 1 {
				f.t.Fatalf("messages per token = %v, want exactly one to each live device", got)
			}
		}},
		{name: "device_session_expired", kind: "comment", reason: skipNoDevice, setup: func(f *pdx, _ string) {
			f.exec(`UPDATE sessions SET expires_at = ? WHERE id = ?`, f.ts(time.Minute), f.devSess)
		}},

		// ordering: when several gates fail the recorded reason is the privacy one
		{name: "privacy_reason_wins_over_preference", kind: "comment", reason: skipNotMember, setup: func(f *pdx, id string) {
			removeBob(f, id)
			f.settings(func(s *models.NotificationSettings) { s.PushEnabled = false })
		}},
		{name: "user_reason_wins_over_membership", kind: "comment", reason: skipUserDisabled, setup: func(f *pdx, id string) {
			removeBob(f, id)
			f.exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, f.ts(time.Minute), f.bob.ID)
		}},
		{name: "membership_reason_wins_over_item_gone", kind: "assigned", reason: skipNotMember, setup: func(f *pdx, id string) {
			removeBob(f, id)
			must(f.t, f.srv.store.DeleteItem(f.item.ID))
		}},
		{name: "workspace_gone_reason_wins_over_item_gone", kind: "assigned", reason: skipWorkspaceGone, setup: func(f *pdx, _ string) {
			must(f.t, f.srv.store.DeleteItem(f.item.ID))
			must(f.t, f.srv.store.DeleteWorkspace(f.ws.Slug))
		}},
		{name: "item_gone_reason_wins_over_visibility", kind: "assigned", reason: skipItemGone, setup: func(f *pdx, id string) {
			specificNone(f, id)
			must(f.t, f.srv.store.DeleteItem(f.item.ID))
		}},
		{name: "visibility_reason_wins_over_assignment", kind: "assigned", reason: skipNotVisible, setup: func(f *pdx, id string) {
			specificNone(f, id)
			f.exec(`UPDATE items SET assigned_user_id = ? WHERE id = ?`, f.owner.ID, f.item.ID)
		}},
	}
}

func TestPushDispatch_Gates(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		for _, c := range gateCases() {
			t.Run(c.name, func(t *testing.T) {
				f := newPdx(t, srv)
				kind := c.kind
				id := f.row(kind, c.row...)
				if c.setup != nil {
					c.setup(f, id)
				}
				f.tick()
				st := f.state(id)
				if c.reason == "" {
					if st.State != "sent" || f.fake.callCount() != 1 {
						t.Fatalf("control: state=%s/%q calls=%d, want sent and 1 call", st.State, st.Reason, f.fake.callCount())
					}
					if c.check != nil {
						c.check(f, id)
					}
					return
				}
				if st.State != "skipped" || st.Reason != c.reason {
					t.Fatalf("state=%s reason=%q, want skipped/%q", st.State, st.Reason, c.reason)
				}
				if n := f.fake.callCount(); n != 0 {
					t.Fatalf("gateway called %d times for a skipped row", n)
				}
			})
		}
	})
}

// Shared FCM token: push_devices is unique on (user, token), so two users can
// hold one phone's token. Only the newest LIVE registrant owns it.
func TestPushDispatch_SharedTokenNeverCrossesUsers(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		carol := f.user("carol")
		must(t, srv.store.AddWorkspaceMember(f.ws.ID, carol.ID, "editor"))
		f.exec(`DELETE FROM push_devices WHERE user_id = ?`, f.bob.ID)
		f.device(f.bob, "tok-phone", 3*time.Hour)
		_, carolSess := f.device(carol, "tok-phone", time.Hour) // carol signed in later
		bobRow := f.row("comment")
		carolRow := f.row("comment", func(o *rowOpt) { o.user = carol })
		f.tick()
		if st := f.state(bobRow); st.State != "skipped" || st.Reason != skipNoDevice {
			t.Fatalf("bob's row = %s/%q, want skipped/no_device: the phone is carol's now", st.State, st.Reason)
		}
		if st := f.state(carolRow); st.State != "sent" {
			t.Fatalf("carol's row = %s/%q", st.State, st.Reason)
		}
		for _, m := range f.fake.all() {
			if m.Data.NID == bobRow {
				t.Fatalf("bob's notification went out to the shared token")
			}
		}
		if n := len(f.fake.all()); n != 1 {
			t.Fatalf("%d messages, want carol's one", n)
		}

		// Carol's session ends: the phone is bob's again.
		f.exec(`UPDATE sessions SET expires_at = ? WHERE id = ?`, f.ts(time.Minute), carolSess)
		again := f.row("comment")
		f.tick()
		if st := f.state(again); st.State != "sent" {
			t.Fatalf("after carol's session expired bob's row = %s/%q, want sent", st.State, st.Reason)
		}
	})
}

// A comment row and a mention row for one comment are two rows of one group:
// both settle sent, neither is dropped as a duplicate, and they share ONE
// push whose kind is the higher-priority one (mention) on a created_at tie.
func TestPushDispatch_MentionAndCommentRowsShareOnePush(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		same := f.ts(2 * time.Minute)
		mk := func(kind string) string {
			id := f.row(kind)
			f.exec(`UPDATE user_notifications SET created_at = ? WHERE id = ?`, same, id)
			return id
		}
		c, m := mk("comment"), mk("mention")
		f.tick()
		for _, id := range []string{c, m} {
			if st := f.state(id); st.State != "sent" {
				t.Fatalf("row %s = %s/%q, want sent: neither is a duplicate of the other", id, st.State, st.Reason)
			}
		}
		msgs := f.fake.all()
		if len(msgs) != 1 || msgs[0].Data.Kind != "mention" || msgs[0].Data.NID != m || msgs[0].Data.Count != 1 {
			t.Fatalf("messages = %+v, want one mention push carrying the mention row's id", msgs)
		}

		// A newer comment row beats an older mention row on created_at.
		older := f.row("mention", func(o *rowOpt) { o.age = 3 * time.Minute })
		newer := f.row("comment", func(o *rowOpt) { o.age = 2 * time.Minute })
		f.tick()
		msgs = f.fake.all()
		last := msgs[len(msgs)-1]
		if last.Data.Kind != "comment" || last.Data.NID != newer {
			t.Fatalf("last push = %+v, want the newer comment row (%s, not %s)", last.Data, newer, older)
		}
	})
}

// gateLookups is every lookup a tick makes that the fixture can force to fail:
// the table whose rename breaks it, and the access it needs to be reached.
var gateLookups = []struct {
	name, table string
	specific    bool // bob needs "specific" collection access to reach it
}{
	{"settings", "user_notification_settings", false},
	{"comment", "comments", false},
	{"user", "users", false},
	{"membership", "workspace_members", false},
	{"workspace", "workspaces", false},
	{"subject", "items", false},
	{"devices", "push_devices", false},
	{"visibility_collection_grants", "collection_grants", true},
	{"visibility_item_grants", "item_grants", true},
	{"visibility_member_collections", "member_collection_access", true},
}

// breakTable makes every query on table fail and returns the undo.
func (f *pdx) breakTable(table string) (restore func()) {
	f.exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_broken`)
	restored := false
	restore = func() {
		if !restored {
			restored = true
			f.exec(`ALTER TABLE ` + table + `_broken RENAME TO ` + table)
		}
	}
	f.t.Cleanup(restore)
	return restore
}

func (f *pdx) grantSpecific() {
	must(f.t, f.srv.store.SetMemberCollectionAccess(f.ws.ID, f.bob.ID, "specific", []string{f.coll.ID}))
}

// A gate that cannot be evaluated is not a denial: the row is unclaimed, no
// attempt is burned, no skip is recorded, and nothing is sent. Every lookup
// the dispatcher makes is forced to fail in turn.
func TestPushDispatch_GateErrorUnclaimsNeverSends(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		for _, lk := range gateLookups {
			t.Run(lk.name, func(t *testing.T) {
				f := newPdx(t, srv)
				if lk.specific {
					f.grantSpecific()
				}
				id := f.row("comment")
				restore := f.breakTable(lk.table)
				f.tick()
				st := f.state(id)
				if st.State != "pending" || st.Claim != "" || st.Attempts != 0 || st.Reason != "" {
					t.Fatalf("after a gate error: %+v, want pending, unclaimed, 0 attempts, no skip reason", st)
				}
				if n := f.fake.callCount(); n != 0 {
					t.Fatalf("gateway called %d times with a gate unevaluable", n)
				}
				restore()
				f.tick()
				if st := f.state(id); st.State != "sent" {
					t.Fatalf("after the gate recovered: %+v, want sent", st)
				}
			})
		}
	})
}

// The dispatcher shares computeWatchAccessVisibility's lookups but not its
// logging: that function logs the workspace id, user id and raw store error,
// and a tick must log none of them (the handler keeps its own log).
func TestPushDispatch_VisibilityErrorsLeakNothingToTheLog(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		for _, lk := range gateLookups {
			if !lk.specific && lk.name != "membership" {
				continue
			}
			t.Run(lk.name, func(t *testing.T) {
				f := newPdx(t, srv)
				f.grantSpecific()
				id := f.row("comment")
				logs := pdCaptureLogs(t)
				f.breakTable(lk.table)
				f.tick()
				out := logs.String()
				for _, leak := range []string{f.ws.ID, f.bob.ID, id, lk.table, "_broken", "no such table", "does not exist", "failed to resolve"} {
					if strings.Contains(out, leak) {
						t.Fatalf("log leaks %q: %s", leak, out)
					}
				}
				if st := f.state(id); st.State != "pending" || st.Claim != "" || st.Attempts != 0 || st.Reason != "" {
					t.Fatalf("row = %+v, want pending, unclaimed, no attempt, no skip", st)
				}
			})
		}
	})
}

// A unit none of whose devices can be given a message (NewMessage refuses an
// unknown kind, which the gates exclude, so the unit is built by hand) is
// settled up front as a skip, whether it is alone in its chunk or another unit
// sends beside it. It is neither unclaimed nor released with an attempt.
func TestPushDispatch_ZeroMessageUnitIsSkipped(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		run := func(t *testing.T, withSender bool) {
			f := newPdx(t, srv)
			other := f.newItem("Other", &f.bob.ID)
			f.exec(`DELETE FROM user_notifications`) // the assignment enqueued a row
			bad := f.row("comment")
			ids := []string{bad}
			good := ""
			if withSender {
				good = f.row("assigned", func(o *rowOpt) { o.item = other })
				ids = append(ids, good)
			}
			claim, err := srv.store.ClaimPendingNotifications("zm", 10, time.Now(), time.Now().Add(-time.Minute), time.Time{}, nil)
			must(t, err)
			if len(claim.Rows) != len(ids) {
				t.Fatalf("claimed %d rows, want %d", len(claim.Rows), len(ids))
			}
			devs, err := srv.store.LivePushDevices(f.bob.ID, time.Now())
			must(t, err)
			chunk := []pushUnit{{rowIDs: []string{bad}, nid: bad, kind: "bogus", devices: devs}}
			if withSender {
				chunk = append(chunk, pushUnit{rowIDs: []string{good}, nid: good, kind: "assigned", devices: devs})
			}
			srv.sendChunk(context.Background(), f.fake, claim.Token, chunk, time.Now().Add(time.Minute), nil, &tickWarn{})
			if st := f.state(bad); st.State != "skipped" || st.Reason != skipNoDevice || st.Attempts != 0 {
				t.Fatalf("zero-message unit = %+v, want skipped/%s, no attempt", st, skipNoDevice)
			}
			wantCalls := 0
			if withSender {
				wantCalls = 1
				if st := f.state(good); st.State != "sent" {
					t.Fatalf("the sending unit = %+v", st)
				}
			}
			if n := f.fake.callCount(); n != wantCalls {
				t.Fatalf("%d Send calls, want %d", n, wantCalls)
			}
		}
		t.Run("alone_in_a_chunk", func(t *testing.T) { run(t, false) })
		t.Run("beside_a_unit_that_sends", func(t *testing.T) { run(t, true) })
	})
}

// The message is content-free: no title, comment text or names, and its
// fields come from the right places.
func TestPushDispatch_MessageShape(t *testing.T) {
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newPdx(t, srv)
		id := f.row("comment")
		f.tick()
		msgs := f.fake.all()
		if len(msgs) != 1 {
			t.Fatalf("%d messages", len(msgs))
		}
		m := msgs[0]
		wantBody, _ := notify.Text("comment", 1)
		if m.Title != notify.Title || m.Body != wantBody || m.Platform != "android" || m.Token != "tok-bob-"+fmt.Sprint(f.n) ||
			m.Data.NID != id || m.Data.CID != "conn-"+f.devID || m.Data.Kind != "comment" || m.Data.Count != 1 {
			t.Fatalf("message = %+v", m)
		}
		// Belt and braces: the exact-field comparison above is the real
		// assertion; this scan only catches a secret that lands in a field
		// the comparison does not name.
		blob := fmt.Sprintf("%+v", msgs)
		for _, secret := range []string{"SECRET", "owner", "Push "} {
			if strings.Contains(blob, secret) {
				t.Fatalf("message carries %q: %s", secret, blob)
			}
		}
	})
}

// user_notifications.kind has a CHECK, so an unknown kind cannot be seeded;
// the guard is for a kind added to the schema before the dispatcher learns its
// text. It must skip, not send.
func TestKindEnabled_UnknownKindIsSkipped(t *testing.T) {
	set := models.DefaultNotificationSettings()
	if got := kindEnabled(set, store.Notification{Kind: "bogus"}); got != skipUnknownKind {
		t.Fatalf("unknown kind = %q, want %q", got, skipUnknownKind)
	}
	for _, k := range []string{"assigned", "comment", "reply", "reminder", "mention"} {
		if got := kindEnabled(set, store.Notification{Kind: k}); got != "" {
			t.Fatalf("%s with default settings = %q, want enabled", k, got)
		}
	}
}
