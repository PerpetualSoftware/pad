package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/notify"
)

// These tests run on SQLite, and on Postgres too under `make test-pg`
// (testStore switches on PAD_TEST_POSTGRES_URL).

func ago(d time.Duration) string   { return time.Now().UTC().Add(-d).Format(time.RFC3339) }
func at(d time.Duration) time.Time { return time.Now().UTC().Add(-d) }

// seedNotif inserts a pending queue row for userID created at createdAt.
func (f notifyFixture) seedNotif(t *testing.T, userID, createdAt string) string {
	t.Helper()
	id := newID()
	f.s.mustExec(t, `INSERT INTO user_notifications (id, user_id, workspace_id, item_id, kind, dedupe_key, created_at)
		VALUES (?, ?, ?, ?, 'comment', ?, ?)`, id, userID, f.ws.ID, f.item.ID, "k-"+id, createdAt)
	return id
}

func (f notifyFixture) notifRow(t *testing.T, id string) (state, reason, token string, attempts int) {
	t.Helper()
	if err := f.s.db.QueryRow(f.s.q(`SELECT push_state, skip_reason, claim_token, attempts FROM user_notifications WHERE id = ?`), id).
		Scan(&state, &reason, &token, &attempts); err != nil {
		t.Fatal(err)
	}
	return
}

// claimRows and claimIDRows are the claim calls with the per-claim token
// dropped, for tests that only look at the rows.
func claimRows(s *Store, token string, limit int, settle, lease time.Time) ([]Notification, error) {
	c, err := s.ClaimPendingNotifications(token, limit, settle, lease, time.Time{}, nil)
	return c.Rows, err
}

func claimIDRows(s *Store, token string, ids []string, settle, lease, ts string) ([]Notification, error) {
	c, err := s.claimNotificationIDs(token, ids, settle, "", lease, ts)
	return c.Rows, err
}

func ids(ns []Notification) map[string]bool {
	m := map[string]bool{}
	for _, n := range ns {
		m[n.ID] = true
	}
	return m
}

// deviceAt registers a device for userID with its own session. ttl is the
// session's remaining life (negative: already expired).
func deviceAt(t *testing.T, s *Store, userID, token, createdAt string, ttl time.Duration) (deviceID, sessionID string) {
	t.Helper()
	plain, err := s.CreateSession(userID, "ios", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(plain))
	if err := s.db.QueryRow(s.q(`SELECT id FROM sessions WHERE token_hash = ?`), hex.EncodeToString(h[:])).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	s.mustExec(t, `UPDATE sessions SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(ttl).Format(time.RFC3339), sessionID)
	deviceID = newID()
	s.mustExec(t, `INSERT INTO push_devices (id, user_id, session_id, connection_id, platform, token, app_version, created_at, updated_at, reg_seq)
		VALUES (?, ?, ?, ?, 'android', ?, '1', ?, ?, `+s.regSeqInsertExpr()+`)`, deviceID, userID, sessionID, "conn-"+deviceID, token, createdAt, createdAt)
	return
}

func deviceIDs(ds []PushDevice) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

func wantIDs(t *testing.T, what string, got []PushDevice, want ...string) {
	t.Helper()
	g := deviceIDs(got)
	if fmt.Sprint(g) != fmt.Sprint(want) && !(len(g) == 0 && len(want) == 0) {
		t.Fatalf("%s: devices = %v, want %v", what, g, want)
	}
}

// Kills: dropping the settle predicate (the fresh row is claimed); dropping the
// state predicate (the sent row is claimed); ignoring limit.
func TestClaimNotifications_SettleStateAndLimit(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	fresh := f.seedNotif(t, u.ID, ago(5*time.Second))
	old1 := f.seedNotif(t, u.ID, ago(30*time.Second))
	old2 := f.seedNotif(t, u.ID, ago(40*time.Second))
	sent := f.seedNotif(t, u.ID, ago(50*time.Second))
	f.s.mustExec(t, `UPDATE user_notifications SET push_state = 'sent' WHERE id = ?`, sent)

	got, err := claimRows(f.s, "tok", 1, at(10*time.Second), at(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != old2 {
		t.Fatalf("limit 1 claimed %v, want oldest pending %s", ids(got), old2)
	}
	got, err = claimRows(f.s, "tok", 10, at(10*time.Second), at(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// "tok" holds old2 already (fresh claim, not reclaimable by anyone, itself
	// included, until the lease lapses). This call returns ONLY what it
	// claimed: old1, not the already-held old2 (TASK-3939 r1-H1).
	m := ids(got)
	if len(got) != 1 || !m[old1] || m[old2] || m[fresh] || m[sent] {
		t.Fatalf("claimed %v, want exactly old1", m)
	}
	if got[0].UserID != u.ID || got[0].WorkspaceID != f.ws.ID || got[0].ItemID != f.item.ID || got[0].Kind != "comment" || got[0].PushState != "pending" {
		t.Fatalf("row not read back whole: %+v", got[0])
	}
}

// Kills: dropping the lease predicate from the candidate select or from the
// claim UPDATE (a live claim is taken over); `<` flipped (a lapsed claim is
// never reclaimed).
func TestClaimNotifications_DisjointClaimers_Lease(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	var all []string
	for i := 0; i < 6; i++ {
		all = append(all, f.seedNotif(t, u.ID, ago(time.Duration(60+i)*time.Second)))
	}
	settle, lease := at(10*time.Second), at(time.Minute)
	x, err := claimRows(f.s, "X", 4, settle, lease)
	if err != nil {
		t.Fatal(err)
	}
	y, err := claimRows(f.s, "Y", 10, settle, lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(x) != 4 || len(y) != 2 {
		t.Fatalf("X got %d, Y got %d, want 4 and 2", len(x), len(y))
	}
	for id := range ids(x) {
		if ids(y)[id] {
			t.Fatalf("%s claimed by both", id)
		}
	}
	// Fresh claims are not reclaimable.
	if z, _ := claimRows(f.s, "Z", 10, settle, lease); len(z) != 0 {
		t.Fatalf("Z took %d rows under a live lease", len(z))
	}
	// A lapsed lease is: with the cutoff an hour ahead, X's rows are stale.
	z, err := claimRows(f.s, "Z", 10, settle, at(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(z) != 6 {
		t.Fatalf("Z reclaimed %d of 6 lapsed rows", len(z))
	}
	_ = all
}

// The claim UPDATE is the arbiter: a candidate list gone stale (another
// claimer took the row in between) must not steal it. Kills: dropping the
// lease predicate from the UPDATE alone.
func TestClaimNotifications_StaleCandidateDoesNotSteal(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	id := f.seedNotif(t, u.ID, ago(time.Minute))
	// Another claimer already holds it, freshly.
	f.s.mustExec(t, `UPDATE user_notifications SET claim_token = 'OTHER', claimed_at = ? WHERE id = ?`, now(), id)
	if got, err := claimIDRows(f.s, "MINE", []string{id}, ago(10*time.Second), ago(time.Minute), now()); err != nil || len(got) != 0 {
		t.Fatalf("stole a held row: %v, %v", ids(got), err)
	}
	// A candidate that has since been settled, or is younger than the settle
	// cutoff, is not claimed either. Kills: dropping the UPDATE's state or
	// settle predicate.
	sentID := f.seedNotif(t, u.ID, ago(time.Minute))
	f.s.mustExec(t, `UPDATE user_notifications SET push_state = 'sent' WHERE id = ?`, sentID)
	freshID := f.seedNotif(t, u.ID, ago(2*time.Second))
	if got, err := claimIDRows(f.s, "MINE", []string{sentID, freshID}, ago(10*time.Second), ago(time.Minute), now()); err != nil || len(got) != 0 {
		t.Fatalf("claimed a stale candidate: %v, %v", ids(got), err)
	}
	for _, id := range []string{sentID, freshID} {
		if _, _, tok, _ := f.notifRow(t, id); tok != "" {
			t.Fatalf("stale candidate %s was claimed by %q", id, tok)
		}
	}
	// And through the real path: a claimer sees nothing.
	if got, _ := claimRows(f.s, "MINE", 10, at(10*time.Second), at(time.Minute)); len(got) != 0 {
		t.Fatalf("claimed a held row: %v", ids(got))
	}
	if _, _, tok, _ := f.notifRow(t, id); tok != "OTHER" {
		t.Fatalf("claim_token = %q", tok)
	}
}

// Kills: dropping `claim_token = ?` from the settle (a stale claimer stamps
// the new holder's row); dropping `push_state = 'pending'` (sent -> skipped).
func TestMarkNotifications_StaleClaimerIsNoOp(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	id := f.seedNotif(t, u.ID, ago(time.Minute))
	settle := at(10 * time.Second)
	xc, _ := f.s.ClaimPendingNotifications("X", 10, settle, at(time.Minute), time.Time{}, nil)
	if len(xc.Rows) != 1 {
		t.Fatal("X claimed nothing")
	}
	// X's lease lapses; Z takes the row.
	zc, _ := f.s.ClaimPendingNotifications("Z", 10, settle, at(-time.Hour), time.Time{}, nil)
	if len(zc.Rows) != 1 {
		t.Fatal("Z did not reclaim")
	}
	for name, mark := range map[string]func() (int64, error){
		"sent":     func() (int64, error) { return f.s.MarkNotificationsSent(xc.Token, []string{id}) },
		"skipped":  func() (int64, error) { return f.s.MarkNotificationsSkipped(xc.Token, []string{id}, "stale") },
		"released": func() (int64, error) { return f.s.ReleaseNotifications(xc.Token, []string{id}) },
	} {
		n, err := mark()
		if err != nil || n != 0 {
			t.Fatalf("stale X %s: n=%d err=%v, want a no-op", name, n, err)
		}
	}
	state, _, tok, attempts := f.notifRow(t, id)
	if state != "pending" || tok != zc.Token || attempts != 0 {
		t.Fatalf("row = %s/%s/%d after stale marks", state, tok, attempts)
	}
	if n, err := f.s.MarkNotificationsSent(zc.Token, []string{id}); err != nil || n != 1 {
		t.Fatalf("Z mark sent: %d %v", n, err)
	}
	// Settled rows stay settled.
	if n, _ := f.s.MarkNotificationsSkipped(zc.Token, []string{id}, "late"); n != 0 {
		t.Fatal("a sent row was re-skipped")
	}
	state, reason, _, _ := f.notifRow(t, id)
	if state != "sent" || reason != "" {
		t.Fatalf("row = %s/%q", state, reason)
	}
	var sentAt string
	f.s.db.QueryRow(f.s.q(`SELECT COALESCE(sent_at,'') FROM user_notifications WHERE id = ?`), id).Scan(&sentAt)
	if sentAt == "" {
		t.Fatal("sent_at not stamped")
	}
}

// Kills: dropping the empty-token guard (an empty token matches every
// unclaimed row).
func TestNotifications_EmptyClaimTokenRefused(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	id := f.seedNotif(t, u.ID, ago(time.Minute))
	if _, err := claimRows(f.s, "", 10, at(10*time.Second), at(time.Minute)); err == nil {
		t.Error("claim with an empty token succeeded")
	}
	if n, err := f.s.MarkNotificationsSent("", []string{id}); err == nil || n != 0 {
		t.Errorf("sent with an empty token: %d %v", n, err)
	}
	if n, err := f.s.MarkNotificationsSkipped("", []string{id}, "x"); err == nil || n != 0 {
		t.Errorf("skipped with an empty token: %d %v", n, err)
	}
	if n, err := f.s.ReleaseNotifications("", []string{id}); err == nil || n != 0 {
		t.Errorf("release with an empty token: %d %v", n, err)
	}
	if state, _, _, _ := f.notifRow(t, id); state != "pending" {
		t.Fatalf("state = %s", state)
	}
}

func TestMarkNotificationsSkipped_RecordsReason(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	a, b := f.seedNotif(t, u.ID, ago(time.Minute)), f.seedNotif(t, u.ID, ago(time.Minute))
	xc, _ := f.s.ClaimPendingNotifications("X", 10, at(10*time.Second), at(time.Minute), time.Time{}, nil)
	if n, err := f.s.MarkNotificationsSkipped(xc.Token, []string{a}, "not_member"); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if state, reason, _, _ := f.notifRow(t, a); state != "skipped" || reason != "not_member" {
		t.Fatalf("a = %s/%s", state, reason)
	}
	if state, _, _, _ := f.notifRow(t, b); state != "pending" {
		t.Fatalf("b = %s, want untouched", state)
	}
}

// Kills: attempts not incremented; the cap changed (5 -> 6 or 4); the skip not
// in the same statement as the fifth release; the claim not cleared.
func TestReleaseNotifications_SkipsAtFive(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	id := f.seedNotif(t, u.ID, ago(time.Minute))
	for attempt := 1; attempt <= MaxNotificationAttempts; attempt++ {
		got, err := f.s.ClaimPendingNotifications("X", 10, at(10*time.Second), at(time.Minute), time.Time{}, nil)
		if err != nil || len(got.Rows) != 1 {
			t.Fatalf("attempt %d: claim %v %v", attempt, ids(got.Rows), err)
		}
		if n, err := f.s.ReleaseNotifications(got.Token, []string{id}); err != nil || n != 1 {
			t.Fatalf("attempt %d: release %d %v", attempt, n, err)
		}
		state, reason, tok, attempts := f.notifRow(t, id)
		if attempts != attempt || tok != "" {
			t.Fatalf("attempt %d: attempts=%d token=%q", attempt, attempts, tok)
		}
		if attempt < MaxNotificationAttempts && (state != "pending" || reason != "") {
			t.Fatalf("attempt %d: %s/%q, want pending", attempt, state, reason)
		}
		if attempt == MaxNotificationAttempts && (state != "skipped" || reason != "gateway_failed") {
			t.Fatalf("attempt %d: %s/%q, want skipped/gateway_failed", attempt, state, reason)
		}
	}
	if got, _ := claimRows(f.s, "X", 10, at(10*time.Second), at(time.Minute)); len(got) != 0 {
		t.Fatal("a gateway_failed row was claimed again")
	}
}

// Kills: dropping the createdCutoff predicate; dropping the lease exclusion (a
// live claim expired under its claimer); dropping the pending predicate.
func TestExpireStaleNotifications(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	old := f.seedNotif(t, u.ID, ago(2*time.Hour))
	recent := f.seedNotif(t, u.ID, ago(30*time.Minute))
	heldOld := f.seedNotif(t, u.ID, ago(2*time.Hour))
	lapsedOld := f.seedNotif(t, u.ID, ago(2*time.Hour))
	sentOld := f.seedNotif(t, u.ID, ago(2*time.Hour))
	f.s.mustExec(t, `UPDATE user_notifications SET claim_token = 'H', claimed_at = ? WHERE id = ?`, now(), heldOld)
	f.s.mustExec(t, `UPDATE user_notifications SET claim_token = 'L', claimed_at = ? WHERE id = ?`, ago(10*time.Minute), lapsedOld)
	f.s.mustExec(t, `UPDATE user_notifications SET push_state = 'sent' WHERE id = ?`, sentOld)

	n, err := f.s.ExpireStaleNotifications(at(time.Hour), at(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expired %d, want 2 (old and lapsedOld)", n)
	}
	for id, want := range map[string]string{old: "expired", lapsedOld: "expired", recent: "pending", heldOld: "pending", sentOld: "sent"} {
		if state, _, _, _ := f.notifRow(t, id); state != want {
			t.Errorf("row %s = %s, want %s", id, state, want)
		}
	}
}

// Kills: dropping the `sent` predicate; dropping `user_id`; dropping `>=`.
func TestCountNotificationsSentSince(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a, b := notifyUser(t, f.s, "a@example.com"), notifyUser(t, f.s, "b@example.com")
	mk := func(user, state string, sentAgo time.Duration) {
		id := f.seedNotif(t, user, ago(3*time.Hour))
		f.s.mustExec(t, `UPDATE user_notifications SET push_state = ?, sent_at = ? WHERE id = ?`, state, ago(sentAgo), id)
	}
	mk(a.ID, "sent", 10*time.Minute)
	mk(a.ID, "sent", 59*time.Minute)
	mk(a.ID, "sent", 2*time.Hour)      // outside the window
	mk(a.ID, "skipped", 5*time.Minute) // not a push
	mk(b.ID, "sent", 5*time.Minute)    // someone else
	got, err := f.s.CountNotificationsSentSince(a.ID, at(time.Hour))
	if err != nil || got != 2 {
		t.Fatalf("count = %d, %v, want 2", got, err)
	}
}

// The window is closed at its start: a row sent exactly at since counts, one
// second earlier does not. Kills: `sent_at >= ?` weakened to `>`.
func TestCountNotificationsSentSince_ExactBoundary(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a := notifyUser(t, f.s, "a@example.com")
	since := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	mk := func(sentAt time.Time) {
		id := f.seedNotif(t, a.ID, ago(3*time.Hour))
		f.s.mustExec(t, `UPDATE user_notifications SET push_state = 'sent', sent_at = ? WHERE id = ?`, sentAt.Format(time.RFC3339), id)
	}
	mk(since)
	if got, err := f.s.CountNotificationsSentSince(a.ID, since); err != nil || got != 1 {
		t.Fatalf("a row sent exactly at since: count = %d, %v, want 1", got, err)
	}
	mk(since.Add(-time.Second))
	if got, err := f.s.CountNotificationsSentSince(a.ID, since); err != nil || got != 1 {
		t.Fatalf("a row sent a second before since: count = %d, %v, want still 1", got, err)
	}
}

// Kills: dropping the pending guard (an old pending row is deleted); dropping
// the created_at predicate (a young settled row is deleted).
func TestPruneNotifications(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	week := 7 * 24 * time.Hour
	oldState := map[string]string{}
	for _, st := range []string{"sent", "skipped", "expired", "pending"} {
		id := f.seedNotif(t, u.ID, ago(week+time.Hour))
		f.s.mustExec(t, `UPDATE user_notifications SET push_state = ? WHERE id = ?`, st, id)
		oldState[st] = id
	}
	young := f.seedNotif(t, u.ID, ago(week-time.Hour))
	f.s.mustExec(t, `UPDATE user_notifications SET push_state = 'sent' WHERE id = ?`, young)
	youngPending := f.seedNotif(t, u.ID, ago(time.Minute))

	n, err := f.s.PruneNotifications(at(week))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("pruned %d, want the 3 settled rows older than 7 days", n)
	}
	exists := func(id string) bool {
		return f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE id = ?`, id) == 1
	}
	for _, st := range []string{"sent", "skipped", "expired"} {
		if exists(oldState[st]) {
			t.Errorf("old %s row survived", st)
		}
	}
	if !exists(oldState["pending"]) || !exists(young) || !exists(youngPending) {
		t.Error("pruned a pending row, or a row younger than the cutoff")
	}
}

// Every guard in the device query has a leg that goes red without it.
func TestLivePushDevices(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	a, b := notifyUser(t, s, "a@example.com"), notifyUser(t, s, "b@example.com")
	now := time.Now().UTC()
	if got, err := s.LivePushDevices(a.ID, now); err != nil || len(got) != 0 {
		t.Fatalf("no devices: %v %v", got, err)
	}
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	// Earliest first, ties by id; other users' devices excluded.
	d3, _ := deviceAt(t, s, a.ID, "t3", stamp(-time.Minute), time.Hour)
	d1, _ := deviceAt(t, s, a.ID, "t1", stamp(-3*time.Hour), time.Hour)
	d2, _ := deviceAt(t, s, a.ID, "t2", stamp(-2*time.Hour), time.Hour)
	deviceAt(t, s, b.ID, "tb", stamp(-5*time.Hour), time.Hour)
	got, err := s.LivePushDevices(a.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs(t, "ordering", got, d1, d2, d3)
	if got[0].Token != "t1" || got[0].ConnectionID == "" || got[0].Platform != "android" {
		t.Fatalf("device not read whole: %+v", got[0])
	}

	// An expired session: no longer live. Kills: dropping s.expires_at.
	dx, _ := deviceAt(t, s, a.ID, "tx", stamp(-4*time.Hour), -time.Minute)
	got, _ = s.LivePushDevices(a.ID, now)
	wantIDs(t, "expired session", got, d1, d2, d3)
	_ = dx

	// A session whose user_id differs from the device's. Kills: dropping
	// s.user_id = d.user_id.
	dm, sm := deviceAt(t, s, a.ID, "tm", stamp(-6*time.Hour), time.Hour)
	s.mustExec(t, `UPDATE sessions SET user_id = ? WHERE id = ?`, b.ID, sm)
	got, _ = s.LivePushDevices(a.ID, now)
	wantIDs(t, "session user mismatch", got, d1, d2, d3)
	_ = dm

	// A shared token: A's older device, B's newer registration of the same
	// token. Only the newer holder gets it. Kills: dropping the clause.
	dShared, _ := deviceAt(t, s, a.ID, "shared", stamp(-10*time.Hour), time.Hour)
	dSharedB, _ := deviceAt(t, s, b.ID, "shared", stamp(-9*time.Hour), time.Hour)
	got, _ = s.LivePushDevices(a.ID, now)
	wantIDs(t, "shared token, A older", got, d1, d2, d3)
	gotB, _ := s.LivePushDevices(b.ID, now)
	if !containsDevice(gotB, dSharedB) {
		t.Fatalf("B lost the shared token it owns: %v", deviceIDs(gotB))
	}
	_ = dShared

	// The newer registrant's session expired: the older holder gets the token
	// back. Kills: dropping s2.expires_at.
	s.mustExec(t, `UPDATE sessions SET expires_at = ? WHERE id = (SELECT session_id FROM push_devices WHERE id = ?)`, stamp(-time.Minute), dSharedB)
	got, _ = s.LivePushDevices(a.ID, now)
	if !containsDevice(got, dShared) {
		t.Fatalf("A did not get the token back when B's session expired: %v", deviceIDs(got))
	}
	// ...and again if B's session belongs to someone else. Kills: dropping
	// s2.user_id = d2.user_id.
	s.mustExec(t, `UPDATE sessions SET expires_at = ?, user_id = ? WHERE id = (SELECT session_id FROM push_devices WHERE id = ?)`,
		stamp(time.Hour), a.ID, dSharedB)
	got, _ = s.LivePushDevices(a.ID, now)
	if !containsDevice(got, dShared) {
		t.Fatalf("A lost the token to a device whose session is not B's: %v", deviceIDs(got))
	}

	// A tie on created_at goes to the later registration (higher reg_seq, TASK-4031),
	// whatever the ids, for exactly one of the two.
	tieA, _ := deviceAt(t, s, a.ID, "tie", stamp(-time.Hour), time.Hour)
	tieB, _ := deviceAt(t, s, b.ID, "tie", stamp(-time.Hour), time.Hour)
	gotA, _ := s.LivePushDevices(a.ID, now)
	gotB2, _ := s.LivePushDevices(b.ID, now)
	ownsA, ownsB := containsDevice(gotA, tieA), containsDevice(gotB2, tieB)
	if ownsA == ownsB {
		t.Fatalf("tie: A owns=%v B owns=%v, want exactly one", ownsA, ownsB)
	}
	if !ownsB {
		t.Fatalf("tie went to the earlier registration")
	}
}

func containsDevice(ds []PushDevice, id string) bool {
	for _, d := range ds {
		if d.ID == id {
			return true
		}
	}
	return false
}

func TestDeletePushDeviceByID(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	a := notifyUser(t, s, "a@example.com")
	d1, _ := deviceAt(t, s, a.ID, "t1", ago(time.Hour), time.Hour)
	d2, _ := deviceAt(t, s, a.ID, "t2", ago(time.Hour), time.Hour)
	if err := s.DeletePushDeviceByID(d1); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePushDeviceByID("missing"); err != nil {
		t.Fatalf("absent id errored: %v", err)
	}
	got, _ := s.LivePushDevices(a.ID, time.Now())
	wantIDs(t, "after delete", got, d2)
}

// Kills: dropping `user_id = ?` (another user reads the row).
func TestGetNotificationForUser(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a, b := notifyUser(t, f.s, "a@example.com"), notifyUser(t, f.s, "b@example.com")
	id := f.seedNotif(t, a.ID, ago(time.Minute))
	n, err := f.s.GetNotificationForUser(a.ID, id)
	if err != nil || n.ID != id || n.UserID != a.ID || n.ItemID != f.item.ID || n.Kind != "comment" || n.PushState != "pending" {
		t.Fatalf("%+v %v", n, err)
	}
	if _, err := f.s.GetNotificationForUser(b.ID, id); !errors.Is(err, ErrNotificationNotFound) {
		t.Fatalf("other user: %v", err)
	}
	if _, err := f.s.GetNotificationForUser(a.ID, "missing"); !errors.Is(err, ErrNotificationNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

// Kills: dropping `i.deleted_at IS NULL`; dropping `c.deleted_at IS NULL`;
// turning the join into one that ignores the collection.
func TestNotificationSubject(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	item, err := f.s.NotificationSubject(f.item.ID)
	if err != nil || item.ID != f.item.ID {
		t.Fatalf("live: %v %v", item, err)
	}
	if _, err := f.s.NotificationSubject("missing"); !errors.Is(err, ErrNotificationSubjectGone) {
		t.Fatalf("missing: %v", err)
	}
	gone := createTestItem(t, f.s, f.ws.ID, f.coll.ID, "Gone", "")
	if err := f.s.DeleteItem(gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.NotificationSubject(gone.ID); !errors.Is(err, ErrNotificationSubjectGone) {
		t.Fatalf("soft-deleted item: %v", err)
	}
	coll2 := createTestCollection(t, f.s, f.ws.ID, "Archived")
	inArchived := createTestItem(t, f.s, f.ws.ID, coll2.ID, "Inside", "")
	if err := f.s.DeleteCollection(coll2.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.NotificationSubject(inArchived.ID); !errors.Is(err, ErrNotificationSubjectGone) {
		t.Fatalf("archived collection: %v", err)
	}
}

// A claimer that settled a row and then claims again reads back only the new
// claim, not its own sent row (the first claim's token stays on a settled row).
// Kills: dropping `push_state = 'pending'` from the read-back.
func TestClaimNotifications_ReadBackExcludesSettled(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	first := f.seedNotif(t, u.ID, ago(2*time.Minute))
	settle, lease := at(10*time.Second), at(time.Minute)
	fc, _ := f.s.ClaimPendingNotifications("X", 10, settle, lease, time.Time{}, nil)
	if len(fc.Rows) != 1 {
		t.Fatal("first claim")
	}
	if _, err := f.s.MarkNotificationsSent(fc.Token, []string{first}); err != nil {
		t.Fatal(err)
	}
	second := f.seedNotif(t, u.ID, ago(time.Minute))
	got, err := claimRows(f.s, "X", 10, settle, lease)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != second {
		t.Fatalf("second claim read back %v, want only %s", ids(got), second)
	}
}

// Held rows must not crowd the limit: the candidate select skips them, so a
// free younger row is still claimed. Kills: dropping the lease predicate from
// the candidate select alone.
func TestClaimNotifications_HeldRowsDoNotCrowdLimit(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	held := f.seedNotif(t, u.ID, ago(5*time.Minute))
	free := f.seedNotif(t, u.ID, ago(time.Minute))
	f.s.mustExec(t, `UPDATE user_notifications SET claim_token = 'OTHER', claimed_at = ? WHERE id = ?`, now(), held)
	got, err := claimRows(f.s, "X", 1, at(10*time.Second), at(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != free {
		t.Fatalf("claimed %v, want only %s", ids(got), free)
	}
}

// A shared token goes to the row with the higher reg_seq, in both directions
// and whatever the ids and timestamps say, and to exactly one user (TASK-4031).
// Kills: comparing updated_at/id instead of reg_seq, or flipping `>`.
func TestLivePushDevices_SharedTokenGoesToHigherRegSeq(t *testing.T) {
	t.Parallel()
	for _, aHigher := range []bool{true, false} {
		t.Run(fmt.Sprintf("A higher seq=%v", aHigher), func(t *testing.T) {
			s := testStore(t)
			a, b := notifyUser(t, s, "a@example.com"), notifyUser(t, s, "b@example.com")
			stamp := ago(time.Hour)
			da, _ := deviceAt(t, s, a.ID, "tie", stamp, time.Hour)
			db, _ := deviceAt(t, s, b.ID, "tie", stamp, time.Hour)
			// The ids point the other way from the sequence.
			lo, hi := "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"
			idA, idB := hi, lo
			seqA, seqB := 1000, 2000
			if aHigher {
				idA, idB = lo, hi
				seqA, seqB = 2000, 1000
			}
			s.mustExec(t, `UPDATE push_devices SET id = ?, reg_seq = ? WHERE id = ?`, idA, seqA, da)
			s.mustExec(t, `UPDATE push_devices SET id = ?, reg_seq = ? WHERE id = ?`, idB, seqB, db)
			gotA, err := s.LivePushDevices(a.ID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			gotB, _ := s.LivePushDevices(b.ID, time.Now())
			if ownsA, ownsB := containsDevice(gotA, idA), containsDevice(gotB, idB); ownsA != aHigher || ownsB != !aHigher {
				t.Fatalf("A owns=%v B owns=%v, want the higher reg_seq (A higher=%v) only", ownsA, ownsB, aHigher)
			}
		})
	}
}

// Kills (TASK-3939 r1-H1): dropping the id predicate from the read-back (a row
// the reused token holds comes back though this call never claimed it; the
// fixed ts makes the claimed_at predicate unable to tell them apart); dropping
// the claimed_at predicate (a stale candidate the same token holds under a
// fresh lease comes back).
func TestClaimNotifications_ReusedTokenReturnsOnlyThisCallsRows(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	held := f.seedNotif(t, u.ID, ago(time.Minute))
	cand := f.seedNotif(t, u.ID, ago(50*time.Second))
	const ts = "2030-01-01T00:00:00Z"
	// Held by the same token, fresh lease, stamped with the SAME ts as the call.
	f.s.mustExec(t, `UPDATE user_notifications SET claim_token = 'tok', claimed_at = ? WHERE id = ?`, ts, held)
	got, err := claimIDRows(f.s, "tok", []string{cand}, ago(10*time.Second), ago(time.Minute), ts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != cand {
		t.Fatalf("got %v, want only %s", ids(got), cand)
	}
	// A stale candidate the token holds under a fresh, older lease is not
	// reclaimed by the UPDATE, and must not be read back either.
	stale := f.seedNotif(t, u.ID, ago(55*time.Second))
	f.s.mustExec(t, `UPDATE user_notifications SET claim_token = 'tok', claimed_at = ? WHERE id = ?`, ago(5*time.Second), stale)
	got, err = claimIDRows(f.s, "tok", []string{stale}, ago(10*time.Second), ago(time.Minute), now())
	if err != nil || len(got) != 0 {
		t.Fatalf("stale candidate read back: %v, %v", ids(got), err)
	}
}

// (a) a reused token already holds a row under a fresh lease: the next call
// neither returns it nor counts it toward limit. (b) more held rows plus new
// candidates than limit: at most limit, only this call's rows. (c) a held row
// whose lease lapsed IS reclaimable by the same token, claimed_at refreshed.
func TestClaimNotifications_ReusedToken(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	settle, lease := at(10*time.Second), at(time.Minute)

	h1 := f.seedNotif(t, u.ID, ago(5*time.Minute))
	h2 := f.seedNotif(t, u.ID, ago(4*time.Minute))
	first, err := claimRows(f.s, "tok", 2, settle, lease)
	if err != nil || len(first) != 2 {
		t.Fatalf("first claim: %v, %v", ids(first), err)
	}
	n1 := f.seedNotif(t, u.ID, ago(3*time.Minute))
	n2 := f.seedNotif(t, u.ID, ago(2*time.Minute))
	n3 := f.seedNotif(t, u.ID, ago(time.Minute))

	// (a)
	got, err := claimRows(f.s, "tok", 1, settle, lease)
	if err != nil || len(got) != 1 || got[0].ID != n1 {
		t.Fatalf("(a) got %v, %v, want only %s", ids(got), err, n1)
	}
	// (b) two held (h1,h2) plus n1 held now, n2,n3 new, limit 1.
	got, err = claimRows(f.s, "tok", 1, settle, lease)
	if err != nil || len(got) != 1 || got[0].ID != n2 {
		t.Fatalf("(b) got %v, %v, want only %s", ids(got), err, n2)
	}
	got, err = claimRows(f.s, "tok", 5, settle, lease)
	if err != nil || len(got) != 1 || got[0].ID != n3 {
		t.Fatalf("(b) got %v, %v, want only %s", ids(got), err, n3)
	}
	// (c) lapse h1's lease: the same token reclaims it, claimed_at refreshed.
	f.s.mustExec(t, `UPDATE user_notifications SET claimed_at = ? WHERE id = ?`, ago(time.Hour), h1)
	got, err = claimRows(f.s, "tok", 5, settle, lease)
	if err != nil || len(got) != 1 || got[0].ID != h1 {
		t.Fatalf("(c) got %v, %v, want only %s", ids(got), err, h1)
	}
	var claimedAt string
	if err := f.s.db.QueryRow(f.s.q(`SELECT claimed_at FROM user_notifications WHERE id = ?`), h1).Scan(&claimedAt); err != nil {
		t.Fatal(err)
	}
	if claimedAt < ago(time.Minute) {
		t.Fatalf("(c) claimed_at not refreshed: %s", claimedAt)
	}
	_ = h2
}

// Two claims that reuse one caller token and stamp the SAME claimed_at (second
// precision) over the same candidates must not read back each other's rows.
// Kills (TASK-3939 r2-F1): no per-call nonce (the per-claim token is the
// caller's, so the loser reads back the winner's rows); reading back by the
// caller token instead of the per-claim token.
func TestClaimNotifications_SameTokenSameSecondIsDisjoint(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	a := f.seedNotif(t, u.ID, ago(time.Minute))
	b := f.seedNotif(t, u.ID, ago(50*time.Second))
	const ts = "2030-01-01T00:00:00Z"
	cands := []string{a, b}
	settle, lease := ago(10*time.Second), ago(time.Minute)
	winner, err := f.s.claimNotificationIDs("tok", cands, settle, "", lease, ts)
	if err != nil {
		t.Fatal(err)
	}
	loser, err := f.s.claimNotificationIDs("tok", cands, settle, "", lease, ts)
	if err != nil {
		t.Fatal(err)
	}
	if len(winner.Rows) != 2 || len(loser.Rows) != 0 {
		t.Fatalf("winner %v, loser %v, want 2 and 0", ids(winner.Rows), ids(loser.Rows))
	}
	if winner.Token == "tok" || winner.Token == "" {
		t.Fatalf("claim token %q is not per-claim", winner.Token)
	}
	for name, mark := range map[string]func() (int64, error){
		"sent":     func() (int64, error) { return f.s.MarkNotificationsSent(loser.Token, cands) },
		"skipped":  func() (int64, error) { return f.s.MarkNotificationsSkipped(loser.Token, cands, "x") },
		"released": func() (int64, error) { return f.s.ReleaseNotifications(loser.Token, cands) },
	} {
		if n, err := mark(); err != nil || n != 0 {
			t.Fatalf("loser %s touched %d rows (err %v)", name, n, err)
		}
	}
	for _, id := range cands {
		if state, _, tok, attempts := f.notifRow(t, id); state != "pending" || tok != winner.Token || attempts != 0 {
			t.Fatalf("%s = %s/%q/%d after the loser's marks", id, state, tok, attempts)
		}
	}
	if n, err := f.s.MarkNotificationsSent(winner.Token, cands); err != nil || n != 2 {
		t.Fatalf("winner mark sent: %d %v", n, err)
	}
	// Two sequential public calls also never share a token.
	c := f.seedNotif(t, u.ID, ago(time.Minute))
	d := f.seedNotif(t, u.ID, ago(time.Minute))
	c1, _ := f.s.ClaimPendingNotifications("tok", 1, at(10*time.Second), at(time.Minute), time.Time{}, nil)
	c2, _ := f.s.ClaimPendingNotifications("tok", 1, at(10*time.Second), at(time.Minute), time.Time{}, nil)
	if c1.Token == c2.Token || len(c1.Rows) != 1 || len(c2.Rows) != 1 || c1.Rows[0].ID == c2.Rows[0].ID {
		t.Fatalf("calls shared a claim: %q %v / %q %v (rows %s %s)", c1.Token, ids(c1.Rows), c2.Token, ids(c2.Rows), c, d)
	}
}

// Kills (TASK-3939 r4-L3, r5-M1): an unguarded limit. A negative LIMIT is
// unbounded on SQLite (every pending row is claimed) and an error on Postgres;
// a limit <= 0 promoted to 1 leases a row nobody asked for; a limit above the
// gateway batch claims more rows than one Send can carry.
func TestClaimNotifications_LimitIsClamped(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	u := notifyUser(t, f.s, "a@example.com")
	for i := 0; i < maxNotificationClaim+2; i++ {
		f.seedNotif(t, u.ID, ago(30*time.Second))
	}
	claimed := func() (n int) {
		t.Helper()
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM user_notifications WHERE claimed_at IS NOT NULL OR claim_token <> ''`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, limit := range []int{0, -1} {
		c, err := f.s.ClaimPendingNotifications("tok", limit, at(10*time.Second), at(time.Minute), time.Time{}, nil)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(c.Rows) != 0 {
			t.Fatalf("limit %d claimed %d rows, want 0", limit, len(c.Rows))
		}
		if n := claimed(); n != 0 {
			t.Fatalf("limit %d wrote %d rows, want none touched", limit, n)
		}
	}
	got, err := claimRows(f.s, "tok", 10*maxNotificationClaim, at(10*time.Second), at(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxNotificationClaim {
		t.Fatalf("limit %d claimed %d rows, want %d", 10*maxNotificationClaim, len(got), maxNotificationClaim)
	}
}

// A pin, not a behaviour test: the claim cap must stay the gateway's batch
// size, so one claim always fits one Send.
func TestMaxNotificationClaimIsGatewayBatch(t *testing.T) {
	if maxNotificationClaim != notify.MaxBatch {
		t.Fatalf("maxNotificationClaim = %d, notify.MaxBatch = %d", maxNotificationClaim, notify.MaxBatch)
	}
}

// TASK-4051 r1 D2: a dispatcher tick claims at most notify.MaxBatch rows, so
// RecordNotificationsSent sees them in one chunk and the tick's recordedPrefix
// is always 0 (internal/server/push_dispatch.go:821). Dropping that call is an
// equivalent mutant only while this holds; raising MaxBatch past the chunk
// makes it a live one, which needs a tick-path test.
func TestNotificationIDChunkHoldsAGatewayBatch(t *testing.T) {
	if notify.MaxBatch > notificationIDChunk {
		t.Fatalf("notify.MaxBatch = %d exceeds notificationIDChunk = %d", notify.MaxBatch, notificationIDChunk)
	}
}
