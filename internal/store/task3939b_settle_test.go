package store

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TASK-3939 part B1: the dispatcher's settle primitives. SQLite, and Postgres
// under `make test-pg`.

// Kills: token-guarding RecordNotificationsSent (the reclaimed leg goes red);
// dropping 'expired' from its state set (the expired leg goes red); letting it
// move a skipped row (the skipped leg goes red).
func TestRecordNotificationsSent_IsByIDForPendingAndExpiredOnly(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "rec@example.com")
	settle := at(10 * time.Second)

	reclaimed := f.seedNotif(t, u.ID, ago(time.Minute))
	xc, _ := f.s.ClaimPendingNotifications("X", 10, settle, at(time.Minute))
	if len(xc.Rows) != 1 {
		t.Fatal("X claimed nothing")
	}
	zc, _ := f.s.ClaimPendingNotifications("Z", 10, settle, at(-time.Hour)) // X's lease lapsed
	if len(zc.Rows) != 1 {
		t.Fatal("Z did not reclaim")
	}
	expired := f.seedNotif(t, u.ID, ago(2*time.Hour))
	f.s.mustExec(t, `UPDATE user_notifications SET push_state = 'expired' WHERE id = ?`, expired)
	skipped := f.seedNotif(t, u.ID, ago(time.Minute))
	f.s.mustExec(t, `UPDATE user_notifications SET push_state = 'skipped', skip_reason = 'not_member' WHERE id = ?`, skipped)

	n, err := f.s.RecordNotificationsSent([]string{reclaimed, expired, skipped})
	if err != nil || n != 2 {
		t.Fatalf("RecordNotificationsSent = %d, %v; want 2 (the reclaimed and the expired row)", n, err)
	}
	for _, c := range []struct{ id, state string }{{reclaimed, "sent"}, {expired, "sent"}, {skipped, "skipped"}} {
		state, _, tok, _ := f.notifRow(t, c.id)
		if state != c.state || (c.state == "sent" && tok != "") {
			t.Fatalf("row %s = %s/%q, want %s with the claim cleared", c.id, state, tok, c.state)
		}
	}
	// The reclaimer's own token-guarded settle is now a no-op.
	if n, _ := f.s.MarkNotificationsSkipped(zc.Token, []string{reclaimed}, "late"); n != 0 {
		t.Fatal("Z's settle changed a row that was recorded sent")
	}
	if n, err := f.s.RecordNotificationsSent(nil); err != nil || n != 0 {
		t.Fatalf("empty ids: %d %v", n, err)
	}
}

// Kills: UnclaimNotifications burning an attempt; clearing nothing; ignoring
// the token guard.
func TestUnclaimNotifications_NoAttemptAndTokenGuarded(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "unc@example.com")
	id := f.seedNotif(t, u.ID, ago(time.Minute))
	c, _ := f.s.ClaimPendingNotifications("X", 10, at(10*time.Second), at(time.Minute))
	if len(c.Rows) != 1 {
		t.Fatal("no claim")
	}
	if n, _ := f.s.UnclaimNotifications("X.not-the-nonce", []string{id}); n != 0 {
		t.Fatal("a foreign token unclaimed the row")
	}
	if n, err := f.s.UnclaimNotifications(c.Token, []string{id}); err != nil || n != 1 {
		t.Fatalf("unclaim = %d, %v", n, err)
	}
	state, _, tok, attempts := f.notifRow(t, id)
	if state != "pending" || tok != "" || attempts != 0 {
		t.Fatalf("row = %s/%q/%d, want pending, unclaimed, 0 attempts", state, tok, attempts)
	}
	if again, _ := f.s.ClaimPendingNotifications("Y", 10, at(10*time.Second), at(time.Minute)); len(again.Rows) != 1 {
		t.Fatal("an unclaimed row is claimable at once")
	}
	if _, err := f.s.UnclaimNotifications("", []string{id}); err == nil {
		t.Fatal("empty token accepted")
	}
}

// Kills: HeldNotifications ignoring the token, or the pending state.
func TestHeldNotifications(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "held@example.com")
	a, b, c := f.seedNotif(t, u.ID, ago(time.Minute)), f.seedNotif(t, u.ID, ago(time.Minute)), f.seedNotif(t, u.ID, ago(time.Minute))
	cl, _ := f.s.ClaimPendingNotifications("X", 10, at(10*time.Second), at(time.Minute))
	if len(cl.Rows) != 3 {
		t.Fatal("claimed", len(cl.Rows))
	}
	f.s.mustExec(t, `UPDATE user_notifications SET push_state = 'sent' WHERE id = ?`, b)
	f.s.mustExec(t, `DELETE FROM user_notifications WHERE id = ?`, c)
	got, err := f.s.HeldNotifications(cl.Token, []string{a, b, c})
	if err != nil || !slices.Equal(got, []string{a}) {
		t.Fatalf("held = %v, %v; want only %s", got, err, a)
	}
	if got, _ := f.s.HeldNotifications("X.other", []string{a}); len(got) != 0 {
		t.Fatalf("another token holds %v", got)
	}
	if _, err := f.s.HeldNotifications("", []string{a}); err == nil {
		t.Fatal("empty token accepted")
	}
}

// Kills: putting a row id back into the claim error text (Q8).
func TestClaimNotificationErrorCarriesNoRowID(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "errtxt@example.com")
	id := f.seedNotif(t, u.ID, ago(time.Minute))
	f.s.mustExec(t, `ALTER TABLE user_notifications RENAME TO user_notifications_x`)
	defer f.s.mustExec(t, `ALTER TABLE user_notifications_x RENAME TO user_notifications`)
	_, err := f.s.claimNotificationIDs("X", []string{id}, cutoffTS(at(10*time.Second)), cutoffTS(at(time.Minute)), now())
	if err == nil {
		t.Fatal("expected an error with the table renamed")
	}
	if strings.Contains(err.Error(), id) {
		t.Fatalf("claim error carries the row id: %v", err)
	}
}
