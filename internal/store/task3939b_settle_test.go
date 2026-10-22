package store

import (
	"errors"
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
	xc, _ := f.s.ClaimPendingNotifications("X", 10, settle, at(time.Minute), nil)
	if len(xc.Rows) != 1 {
		t.Fatal("X claimed nothing")
	}
	zc, _ := f.s.ClaimPendingNotifications("Z", 10, settle, at(-time.Hour), nil) // X's lease lapsed
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
		state, _, tok, attempts := f.notifRow(t, c.id)
		if state != c.state || (c.state == "sent" && tok != "") {
			t.Fatalf("row %s = %s/%q, want %s with the claim cleared", c.id, state, tok, c.state)
		}
		if attempts != 0 {
			t.Fatalf("row %s has %d attempts: a delivery is not a failed attempt", c.id, attempts)
		}
		var sentAt string
		if err := f.s.db.QueryRow(f.s.q(`SELECT COALESCE(sent_at, '') FROM user_notifications WHERE id = ?`), c.id).Scan(&sentAt); err != nil {
			t.Fatal(err)
		}
		if (sentAt != "") != (c.state == "sent") {
			t.Fatalf("row %s (%s) has sent_at %q", c.id, c.state, sentAt)
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
	c, _ := f.s.ClaimPendingNotifications("X", 10, at(10*time.Second), at(time.Minute), nil)
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
	if again, _ := f.s.ClaimPendingNotifications("Y", 10, at(10*time.Second), at(time.Minute), nil); len(again.Rows) != 1 {
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
	cl, _ := f.s.ClaimPendingNotifications("X", 10, at(10*time.Second), at(time.Minute), nil)
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

// Kills: RecordNotificationsSent or HeldNotifications handling only the first
// notificationIDChunk ids of a longer list.
func TestNotificationSettles_CoverMoreIDsThanOneChunk(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "chunk@example.com")
	ids := make([]string, notificationIDChunk*2+7)
	for i := range ids {
		ids[i] = f.seedNotif(t, u.ID, ago(time.Minute))
	}
	f.s.mustExec(t, `UPDATE user_notifications SET claim_token = 'L.n', claimed_at = ? WHERE user_id = ?`, ago(time.Second), u.ID)
	held, err := f.s.HeldNotifications("L.n", ids)
	if err != nil || len(held) != len(ids) {
		t.Fatalf("held %d of %d ids, %v", len(held), len(ids), err)
	}
	if n, err := f.s.RecordNotificationsSent(ids); err != nil || n != int64(len(ids)) {
		t.Fatalf("recorded %d of %d ids, %v", n, len(ids), err)
	}
	last := ids[len(ids)-1]
	if state, _, _, _ := f.notifRow(t, last); state != "sent" {
		t.Fatalf("the last id is %s, want sent", state)
	}
}

// TASK-4032 codex r1 M1. Kills: ignoring exclude (the excluded legs go red);
// building the clause for an empty list (the nil leg goes red on a SQL error).
func TestClaimNotifications_ExcludeSkipsTheListedIDs(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "excl@example.com")
	a := f.seedNotif(t, u.ID, ago(3*time.Minute))
	b := f.seedNotif(t, u.ID, ago(2*time.Minute))
	c := f.seedNotif(t, u.ID, ago(time.Minute))
	settle, lease := at(10*time.Second), at(time.Minute)

	got, err := f.s.ClaimPendingNotifications("X", 2, settle, lease, []string{a})
	if err != nil || len(got.Rows) != 2 || got.Rows[0].ID != b || got.Rows[1].ID != c {
		t.Fatalf("excluding the oldest: %v %v, want %s and %s", ids(got.Rows), err, b, c)
	}
	if st, _, _, _ := f.notifRow(t, a); st != "pending" {
		t.Fatalf("excluded row = %s, want untouched", st)
	}

	// At the dispatcher's cap of 1000 ids, with unrelated ids, nothing is lost.
	many := make([]string, 1000)
	for i := range many {
		many[i] = "nope-" + strings.Repeat("x", 8) + string(rune('a'+i%26)) + time.Duration(i).String()
	}
	got, err = f.s.ClaimPendingNotifications("Y", 10, settle, lease, many)
	if err != nil || len(got.Rows) != 1 || got.Rows[0].ID != a {
		t.Fatalf("1000 unrelated exclusions: %v %v, want just %s (b and c are leased)", ids(got.Rows), err, a)
	}
	if got, err = f.s.ClaimPendingNotifications("Z", 10, settle, lease, []string{}); err != nil || len(got.Rows) != 0 {
		t.Fatalf("empty exclusion: %v %v, want nothing left", ids(got.Rows), err)
	}
}

// TASK-4032 codex r3 M1. A failure in a later chunk follows chunks that
// committed; the error says how many leading ids they covered. Kills:
// returning a plain error (errors.As fails); reporting 0 or len(ids) landed.
func TestRecordNotificationsSent_ReportsTheChunksThatLanded(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "partial@example.com")
	ids := make([]string, notificationIDChunk+3)
	for i := range ids {
		ids[i] = f.seedNotif(t, u.ID, ago(time.Minute))
	}
	bad := ids[notificationIDChunk+1]
	if f.s.dialect.Driver() == DriverPostgres {
		f.s.mustExec(t, `CREATE FUNCTION rs_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rs fault'; END $$`)
		f.s.mustExec(t, `CREATE TRIGGER rs_fail BEFORE UPDATE ON user_notifications FOR EACH ROW WHEN (NEW.push_state = 'sent' AND NEW.id = '`+bad+`') EXECUTE FUNCTION rs_fail()`)
	} else {
		f.s.mustExec(t, `CREATE TRIGGER rs_fail BEFORE UPDATE ON user_notifications WHEN NEW.push_state = 'sent' AND NEW.id = '`+bad+`' BEGIN SELECT RAISE(ABORT, 'rs fault'); END`)
	}
	n, err := f.s.RecordNotificationsSent(ids)
	var rse *RecordSentError
	if !errors.As(err, &rse) {
		t.Fatalf("err = %v, want a *RecordSentError", err)
	}
	if rse.Landed != notificationIDChunk || n != notificationIDChunk {
		t.Fatalf("landed %d, recorded %d; want %d each (the first chunk)", rse.Landed, n, notificationIDChunk)
	}
	if st, _, _, _ := f.notifRow(t, ids[notificationIDChunk-1]); st != "sent" {
		t.Fatalf("last id of the committed chunk is %s, want sent", st)
	}
	if st, _, _, _ := f.notifRow(t, ids[notificationIDChunk]); st != "pending" {
		t.Fatalf("first id of the failed chunk is %s, want pending", st)
	}
}
