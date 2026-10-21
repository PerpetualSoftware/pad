package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TASK-3938. These run on SQLite, and on Postgres whenever PAD_TEST_POSTGRES_URL
// is set (testStore switches), so `make test-pg` is the Postgres half.

// pushSession makes a live session for the user and returns its id and token.
func pushSession(t *testing.T, s *Store, userID string) (id, token string) {
	t.Helper()
	tok, err := s.CreateSession(userID, "web", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.ValidateSession(tok)
	if err != nil || info == nil {
		t.Fatalf("ValidateSession: %v %v", info, err)
	}
	return info.ID, tok
}

func register(t *testing.T, s *Store, userID, sessionID, token string) *PushDevice {
	t.Helper()
	d, err := s.RegisterPushDevice(userID, sessionID, "conn-"+sessionID[:8], "android", token, "1.0")
	if err != nil {
		t.Fatalf("RegisterPushDevice(%s): %v", token, err)
	}
	return d
}

func deviceTokens(t *testing.T, s *Store, userID string) map[string]string {
	t.Helper()
	ds, err := s.ListPushDevices(userID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, d := range ds {
		out[d.Token] = d.SessionID
	}
	return out
}

// Kills: a ValidateSession that does not select the id.
func TestPushDevices_ValidateSession_ReturnsID(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "vid@example.com")
	id, tok := pushSession(t, s, u.ID)
	sum := sha256.Sum256([]byte(tok))
	var want string
	if err := s.db.QueryRow(s.q(`SELECT id FROM sessions WHERE token_hash = ?`), hex.EncodeToString(sum[:])).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if id == "" || id != want {
		t.Fatalf("SessionInfo.ID = %q, want the row id %q", id, want)
	}
}

// Kills: a plain INSERT, or a (user, token)-only delete: the old session is
// ALIVE and still holds the token.
func TestPushDevices_Register_SameTokenNewSession(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "a@example.com")
	oldSess, _ := pushSession(t, s, u.ID)
	newSess, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, oldSess, "tok-A")
	register(t, s, u.ID, newSess, "tok-A")
	got := deviceTokens(t, s, u.ID)
	if len(got) != 1 || got["tok-A"] != newSess {
		t.Fatalf("devices = %v, want exactly tok-A on the new session", got)
	}
}

// Kills: a (user, token)-only delete, which leaves the session's old row and
// collides on session_id.
func TestPushDevices_Register_NewTokenSameSession(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "b@example.com")
	sess, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, sess, "tok-A")
	register(t, s, u.ID, sess, "tok-B")
	got := deviceTokens(t, s, u.ID)
	if len(got) != 1 || got["tok-B"] != sess {
		t.Fatalf("devices = %v, want exactly tok-B on the session", got)
	}
}

// The ruling's case (c): the caller's session holds token A, another session
// of the same user holds token B, and the caller now registers B. Kills
// dropping EITHER arm of the delete.
func TestPushDevices_Register_BothKeys(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "c@example.com")
	caller, _ := pushSession(t, s, u.ID)
	other, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, caller, "tok-A")
	register(t, s, u.ID, other, "tok-B")
	register(t, s, u.ID, caller, "tok-B")
	got := deviceTokens(t, s, u.ID)
	if len(got) != 1 || got["tok-B"] != caller {
		t.Fatalf("devices = %v, want exactly tok-B on the caller's session", got)
	}
}

// Another user holding the same token string is not touched.
func TestPushDevices_Register_OtherUsersTokenUntouched(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u1, u2 := notifyUser(t, s, "u1@example.com"), notifyUser(t, s, "u2@example.com")
	s1, _ := pushSession(t, s, u1.ID)
	s2, _ := pushSession(t, s, u2.ID)
	register(t, s, u1.ID, s1, "shared")
	register(t, s, u2.ID, s2, "shared")
	if len(deviceTokens(t, s, u1.ID)) != 1 || len(deviceTokens(t, s, u2.ID)) != 1 {
		t.Fatal("registering one user's token removed another user's")
	}
}

// Kills: no eviction (the 21st device is simply added), evicting the newest,
// or evicting by something other than updated_at.
func TestPushDevices_Cap20_EvictsOldest(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "cap@example.com")
	sessions := make([]string, MaxPushDevicesPerUser)
	for i := range sessions {
		sessions[i], _ = pushSession(t, s, u.ID)
		register(t, s, u.ID, sessions[i], fmt.Sprintf("tok-%02d", i))
	}
	// Dates 0..19 by updated_at, oldest first, but with tok-07 the oldest: a
	// register order that is not the age order.
	base := time.Now().UTC().Add(-time.Hour)
	for i := range sessions {
		at := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		if i == 7 {
			at = base.Add(-time.Hour).Format(time.RFC3339)
		}
		s.mustExec(t, `UPDATE push_devices SET updated_at = ?, created_at = ? WHERE token = ?`, at, at, fmt.Sprintf("tok-%02d", i))
	}
	extra, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, extra, "tok-new")
	got := deviceTokens(t, s, u.ID)
	if len(got) != MaxPushDevicesPerUser {
		t.Fatalf("%d devices, want the cap %d", len(got), MaxPushDevicesPerUser)
	}
	if _, ok := got["tok-07"]; ok {
		t.Fatal("the oldest device (tok-07) survived the eviction")
	}
	if _, ok := got["tok-new"]; !ok {
		t.Fatal("the new device was not registered")
	}
	// Replacing an existing device at the cap evicts nothing.
	register(t, s, u.ID, extra, "tok-new-2")
	if got := deviceTokens(t, s, u.ID); len(got) != MaxPushDevicesPerUser {
		t.Fatalf("a replace at the cap left %d devices", len(got))
	} else if _, ok := got["tok-00"]; !ok {
		t.Fatal("a replace at the cap evicted another device")
	}
}

// Ties on updated_at break by created_at, then id.
func TestPushDevices_Cap20_TieBreaksByCreatedAt(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "tie@example.com")
	at := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < MaxPushDevicesPerUser; i++ {
		sess, _ := pushSession(t, s, u.ID)
		register(t, s, u.ID, sess, fmt.Sprintf("tok-%02d", i))
		s.mustExec(t, `UPDATE push_devices SET updated_at = ?, created_at = ? WHERE token = ?`,
			at.Format(time.RFC3339), at.Add(time.Duration(19-i)*time.Second).Format(time.RFC3339), fmt.Sprintf("tok-%02d", i))
	}
	extra, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, extra, "tok-new")
	// Same updated_at everywhere; tok-19 has the earliest created_at.
	if _, ok := deviceTokens(t, s, u.ID)["tok-19"]; ok {
		t.Fatal("the tie was not broken by created_at: tok-19 survived")
	}
}

// Kills: removing push_devices.session_id's cascade; the logout path is
// DeleteSession.
func TestPushDevices_LogoutCascades(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "logout@example.com")
	sess, tok := pushSession(t, s, u.ID)
	keep, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, sess, "tok-out")
	register(t, s, u.ID, keep, "tok-keep")
	if err := s.DeleteSession(tok); err != nil {
		t.Fatal(err)
	}
	got := deviceTokens(t, s, u.ID)
	if _, ok := got["tok-out"]; ok || len(got) != 1 {
		t.Fatalf("devices after logout = %v, want only the other session's", got)
	}
}

// Kills: a sweep that leaves the device of an expired session.
func TestPushDevices_ExpiryCascades(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "expiry@example.com")
	sess, _ := pushSession(t, s, u.ID)
	live, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, sess, "tok-exp")
	register(t, s, u.ID, live, "tok-live")
	s.mustExec(t, `UPDATE sessions SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), sess)
	if err := s.CleanExpiredSessions(); err != nil {
		t.Fatal(err)
	}
	got := deviceTokens(t, s, u.ID)
	if _, ok := got["tok-exp"]; ok || len(got) != 1 {
		t.Fatalf("devices after the sweep = %v, want only the live session's", got)
	}
}

// Kills: binding a device to a session that is gone or expired.
func TestPushDevices_Register_GoneOrExpiredSession(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "gone@example.com")
	if _, err := s.RegisterPushDevice(u.ID, "no-such-session", "c1", "ios", "t", ""); !errors.Is(err, ErrPushSessionGone) {
		t.Fatalf("gone session: %v, want ErrPushSessionGone", err)
	}
	sess, _ := pushSession(t, s, u.ID)
	s.mustExec(t, `UPDATE sessions SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), sess)
	if _, err := s.RegisterPushDevice(u.ID, sess, "c1", "ios", "t", ""); !errors.Is(err, ErrPushSessionGone) {
		t.Fatalf("expired session: %v, want ErrPushSessionGone", err)
	}
	// Another user's session is not the caller's.
	other := notifyUser(t, s, "other-gone@example.com")
	os, _ := pushSession(t, s, other.ID)
	if _, err := s.RegisterPushDevice(u.ID, os, "c1", "ios", "t", ""); !errors.Is(err, ErrPushSessionGone) {
		t.Fatalf("another user's session: %v, want ErrPushSessionGone", err)
	}
	if len(deviceTokens(t, s, u.ID))+len(deviceTokens(t, s, other.ID)) != 0 {
		t.Fatal("a refused register left a row")
	}
}

// DeletePushDevice is scoped to the caller's session and connection id, and
// idempotent.
func TestPushDevices_Delete_ScopedAndIdempotent(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "del@example.com")
	a, _ := pushSession(t, s, u.ID)
	b, _ := pushSession(t, s, u.ID)
	da := register(t, s, u.ID, a, "tok-a")
	register(t, s, u.ID, b, "tok-b")
	if err := s.DeletePushDevice(u.ID, b, da.ConnectionID); err != nil {
		t.Fatal(err)
	}
	if len(deviceTokens(t, s, u.ID)) != 2 {
		t.Fatal("deleting through another session's id removed a device")
	}
	for i := 0; i < 2; i++ {
		if err := s.DeletePushDevice(u.ID, a, da.ConnectionID); err != nil {
			t.Fatal(err)
		}
	}
	if got := deviceTokens(t, s, u.ID); len(got) != 1 {
		t.Fatalf("devices = %v, want only tok-b", got)
	}
}

// The caller's device moves to the replacement session; another session's
// device is gone; the old session is gone. Kills: rebinding every session's
// devices, rebinding none, deleting before rebinding.
func TestPushDevices_PasswordChange_RebindsCallerOnly(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "rot@example.com")
	caller, _ := pushSession(t, s, u.ID)
	other, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, caller, "tok-caller")
	register(t, s, u.ID, other, "tok-other")
	tok, err := s.RotateUserSessions(u.ID, u.CredentialEpoch, caller, "web", "", "", time.Hour, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.ValidateSession(tok)
	if err != nil || info == nil {
		t.Fatalf("the replacement session does not validate: %v %v", info, err)
	}
	got := deviceTokens(t, s, u.ID)
	if len(got) != 1 || got["tok-caller"] != info.ID {
		t.Fatalf("devices = %v, want only tok-caller on the replacement %s", got, info.ID)
	}
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`), u.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d sessions after rotation (%v), want 1", n, err)
	}
}

// Kills: dropping the fail-closed fallback: a failed rotation rolls back and
// would leave every old session alive.
func TestPushDevices_Rotation_InjectedFailure_RevokesAll(t *testing.T) {
	s := testStore(t)
	u := notifyUser(t, s, "rotfail@example.com")
	caller, _ := pushSession(t, s, u.ID)
	other, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, caller, "tok-caller")
	register(t, s, u.ID, other, "tok-other")
	restore := s.SetRotateFaultHookForTesting(func() error { return errors.New("injected") })
	defer restore()
	if tok, err := s.RotateUserSessions(u.ID, u.CredentialEpoch, caller, "web", "", "", time.Hour, time.Time{}); err == nil || tok != "" {
		t.Fatalf("an injected failure returned %q, %v", tok, err)
	}
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`), u.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d sessions after a failed rotation (%v), want 0: it must fail closed", n, err)
	}
	if got := deviceTokens(t, s, u.ID); len(got) != 0 {
		t.Fatalf("devices survived the revoke-all: %v", got)
	}
}

// The credential-epoch fence and the active-user check survive the move into
// the transaction, and a refusal revokes too.
func TestPushDevices_Rotation_KeepsTheFence(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "fence@example.com")
	caller, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, caller, "tok")
	if _, err := s.RotateUserSessions(u.ID, u.CredentialEpoch+1, caller, "web", "", "", time.Hour, time.Time{}); !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("a stale epoch: %v, want ErrCredentialsChanged", err)
	}
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`), u.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d sessions after a fenced rotation (%v), want 0", n, err)
	}
}
