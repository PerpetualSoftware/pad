package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// notifyFixture is a workspace with an item and a handful of users, each of
// whom can be given a device.
type notifyFixture struct {
	s    *Store
	ws   *models.Workspace
	coll *models.Collection
	item *models.Item
}

func newNotifyFixture(t *testing.T) notifyFixture {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Notify")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	return notifyFixture{s: s, ws: ws, coll: coll, item: createTestItem(t, s, ws.ID, coll.ID, "Subject", "")}
}

func notifyUser(t *testing.T, s *Store, email string) *models.User {
	t.Helper()
	return createTestUser(t, s, email, email, "password-1234-abcd")
}

// addDevice registers a push device for the user the way the (later) register
// endpoint will: one row bound to a fresh session.
func addDevice(t *testing.T, s *Store, userID string) string {
	t.Helper()
	if _, err := s.CreateSession(userID, "ios", "", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	if err := s.db.QueryRow(s.q(`SELECT id FROM sessions WHERE user_id = ? ORDER BY created_at DESC LIMIT 1`), userID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	ts := now()
	s.mustExec(t, `INSERT INTO push_devices (id, user_id, session_id, connection_id, platform, token, app_version, created_at, updated_at, reg_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, `+s.regSeqInsertExpr()+`)`, newID(), userID, sessionID, "conn-"+newID(), "ios", "tok-"+newID(), "1.0", ts, ts)
	return sessionID
}

func (f notifyFixture) join(t *testing.T, us ...*models.User) {
	t.Helper()
	for _, u := range us {
		if err := f.s.AddWorkspaceMember(f.ws.ID, u.ID, "editor"); err != nil {
			t.Fatal(err)
		}
	}
}

// queueRow inserts a queue row directly, the way the enqueue side (a later
// change) will: A's tests need rows to cascade without that code.
func (f notifyFixture) queueRow(t *testing.T, userID, actorID string) {
	t.Helper()
	var actor any
	if actorID != "" {
		actor = actorID
	}
	f.s.mustExec(t, `INSERT INTO user_notifications (id, user_id, workspace_id, item_id, kind, actor_user_id, dedupe_key, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		newID(), userID, f.ws.ID, f.item.ID, "assigned", actor, "raw:"+newID(), now())
}

// Kills: wrong defaults; a partial update on put; losing a zeroed field.
func TestNotificationSettings_DefaultsAndPut(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "settings@example.com")
	got, err := s.GetNotificationSettings(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != models.DefaultNotificationSettings() {
		t.Fatalf("no row: %+v, want the defaults", got)
	}
	want := models.NotificationSettings{PushEnabled: true, NotifyMentions: true, IncludeAgentActivity: true}
	if err := s.PutNotificationSettings(u.ID, want); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetNotificationSettings(u.ID); got != want {
		t.Fatalf("after put: %+v, want %+v", got, want)
	}
	// A second put is a full replace: every field the first turned on and the
	// second leaves off is off.
	want = models.NotificationSettings{NotifyReplies: true}
	if err := s.PutNotificationSettings(u.ID, want); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetNotificationSettings(u.ID); got != want {
		t.Fatalf("after replace: %+v, want %+v", got, want)
	}
}

func TestDefaultNotificationSettings(t *testing.T) {
	t.Parallel()
	d := models.DefaultNotificationSettings()
	if !(d.PushEnabled && d.NotifyAssigned && d.NotifyComments && d.NotifyReplies && d.NotifyReminders && d.NotifyMentions) || d.IncludeAgentActivity {
		t.Fatalf("defaults = %+v: every kind on, agent activity off", d)
	}
}

// Kills: removing push_devices.session_id's cascade, user_notifications'
// user_id cascade, user_notification_settings' cascade, or the actor's SET
// NULL. NOT push_devices.user_id's cascade: DeleteAccountAtomic deletes the
// user's sessions first, so the device is already gone by the session path.
// TestPushDevices_UserCascade isolates that one.
func TestPushTables_Cascades(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a, b := notifyUser(t, f.s, "a@example.com"), notifyUser(t, f.s, "b@example.com")
	f.join(t, a, b)
	sess := addDevice(t, f.s, b.ID)
	f.queueRow(t, b.ID, a.ID)
	if err := f.s.PutNotificationSettings(b.ID, models.DefaultNotificationSettings()); err != nil {
		t.Fatal(err)
	}

	// The device dies with its session; the queue stays.
	f.s.mustExec(t, `DELETE FROM sessions WHERE id = ?`, sess)
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM push_devices WHERE user_id = ?`, b.ID); n != 0 {
		t.Errorf("device survived its session: %d", n)
	}
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ?`, b.ID); n != 1 {
		t.Errorf("queue rows after the session went = %d, want 1", n)
	}

	// The actor going away keeps the row and clears the reference.
	if err := f.s.DeleteAccountAtomic(a.ID); err != nil {
		t.Fatal(err)
	}
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND actor_user_id IS NULL`, b.ID); n != 1 {
		t.Errorf("row with its actor nulled = %d, want 1", n)
	}

	// The recipient going away takes the queue and the settings.
	addDevice(t, f.s, b.ID)
	if err := f.s.DeleteAccountAtomic(b.ID); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"push_devices", "user_notifications", "user_notification_settings"} {
		if n := f.s.countRows(t, `SELECT COUNT(*) FROM `+tbl+` WHERE user_id = ?`, b.ID); n != 0 {
			t.Errorf("%s kept %d rows for a deleted user", tbl, n)
		}
	}
}

// Kills: dropping push_devices.user_id's cascade. In every real path a
// user's own sessions go first and take the device with them, so the test
// binds the device to ANOTHER user's session: deleting the device's user then
// leaves the session standing, and only the user_id cascade can clear the row.
func TestPushDevices_UserCascade(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner, host := notifyUser(t, s, "owner@example.com"), notifyUser(t, s, "host@example.com")
	sess := addDevice(t, s, host.ID)
	s.mustExec(t, `UPDATE push_devices SET user_id = ? WHERE session_id = ?`, owner.ID, sess)

	s.mustExec(t, `DELETE FROM users WHERE id = ?`, owner.ID)
	if n := s.countRows(t, `SELECT COUNT(*) FROM sessions WHERE id = ?`, sess); n != 1 {
		t.Fatalf("the host's session went too (%d rows), so the session path could have cleared the device", n)
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM push_devices WHERE session_id = ?`, sess); n != 0 {
		t.Fatalf("device survived its user: %d", n)
	}
}

// Kills: dropping the workspace_id cascade.
func TestPushTables_PurgeEmptiesTheQueue(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a, b := notifyUser(t, f.s, "a@example.com"), notifyUser(t, f.s, "b@example.com")
	f.join(t, a, b)
	f.queueRow(t, b.ID, a.ID)
	if err := f.s.DeleteWorkspace(f.ws.Slug); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PurgeWorkspaceData(f.ws.ID); err != nil {
		t.Fatal(err)
	}
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE workspace_id = ?`, f.ws.ID); n != 0 {
		t.Fatalf("%d queue rows survived a workspace purge", n)
	}
}
