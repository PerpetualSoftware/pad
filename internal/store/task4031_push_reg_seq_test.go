package store

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

func regSeqOf(t *testing.T, s *Store, userID, token string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(s.q(`SELECT reg_seq FROM push_devices WHERE user_id = ? AND token = ?`), userID, token).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TASK-4031. Two users register one phone, both rows are forced to the same
// second, and the PREVIOUS registrant (u1) holds the larger id. The old rule
// (updated_at, then id) handed the phone back to u1; the sequence gives it to
// u2, and a later same-second re-register by u1 gives it back to u1.
// Kills: the ownership clause comparing updated_at then id; a keep-row UPDATE
// that does not advance reg_seq.
func TestPushDevices_SameSecondCrossUserReclaim_LatestRegistrantOwns(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u1, u2 := notifyUser(t, s, "sr1@example.com"), notifyUser(t, s, "sr2@example.com")
	s1, _ := pushSession(t, s, u1.ID)
	s2, _ := pushSession(t, s, u2.ID)
	d1 := register(t, s, u1.ID, s1, "T")
	d2 := register(t, s, u2.ID, s2, "T")
	same := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	s.mustExec(t, `UPDATE push_devices SET created_at = ?, updated_at = ? WHERE token = ?`, same, same, "T")
	s.mustExec(t, `UPDATE push_devices SET id = ? WHERE id = ?`, "zzzzzzzz-u1-larger-id", d1.ID)
	s.mustExec(t, `UPDATE push_devices SET id = ? WHERE id = ?`, "aaaaaaaa-u2-smaller-id", d2.ID)
	if !liveHas(t, s, u2.ID, "T") || liveHas(t, s, u1.ID, "T") {
		t.Fatal("u2 registered last, so u2 should own T and u1 should not")
	}
	register(t, s, u1.ID, s1, "T")
	s.mustExec(t, `UPDATE push_devices SET updated_at = ? WHERE token = ?`, same, "T")
	if !liveHas(t, s, u1.ID, "T") || liveHas(t, s, u2.ID, "T") {
		t.Fatal("u1 re-registered last, so u1 should own T and u2 should not")
	}
}

// Kills: a keep-row UPDATE without reg_seq; a re-register that changes id or created_at.
func TestPushDevices_Reregister_AdvancesRegSeq(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "adv@example.com")
	other := notifyUser(t, s, "adv2@example.com")
	sess, _ := pushSession(t, s, u.ID)
	sess2, _ := pushSession(t, s, u.ID)
	so, _ := pushSession(t, s, other.ID)
	first := register(t, s, u.ID, sess, "T")
	before := regSeqOf(t, s, u.ID, "T")
	register(t, s, other.ID, so, "X") // another register in between
	for name, sid := range map[string]string{"same session": sess, "new session": sess2} {
		again := register(t, s, u.ID, sid, "T")
		after := regSeqOf(t, s, u.ID, "T")
		if after <= before {
			t.Fatalf("%s: reg_seq %d -> %d, want strictly greater", name, before, after)
		}
		if again.ID != first.ID || again.CreatedAt != first.CreatedAt {
			t.Fatalf("%s: id/created_at changed: %+v vs %+v", name, again, first)
		}
		before = after
	}
}

// Registers are strictly ordered: each new row's reg_seq exceeds every earlier one.
func TestPushDevices_Register_RegSeqStrictlyIncreases(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "inc@example.com")
	var prev int64
	for i := 0; i < 4; i++ {
		sid, _ := pushSession(t, s, u.ID)
		register(t, s, u.ID, sid, fmt.Sprintf("tok-%d", i))
		cur := regSeqOf(t, s, u.ID, fmt.Sprintf("tok-%d", i))
		if cur <= prev {
			t.Fatalf("register %d: reg_seq %d after %d", i, cur, prev)
		}
		prev = cur
	}
}

// A password-rotation rebind moves the session, it is not a registration.
// Kills: advancing reg_seq in the sessions.go rebind.
func TestPushDevices_Rotation_DoesNotAdvanceRegSeq(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	u := notifyUser(t, s, "rotseq@example.com")
	caller, _ := pushSession(t, s, u.ID)
	register(t, s, u.ID, caller, "tok-caller")
	before := regSeqOf(t, s, u.ID, "tok-caller")
	if _, err := s.RotateUserSessions(u.ID, u.CredentialEpoch, caller, "web", "", "", time.Hour, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if after := regSeqOf(t, s, u.ID, "tok-caller"); after != before {
		t.Fatalf("reg_seq %d -> %d across a rotation, want unchanged", before, after)
	}
}

// Migration 157 / pg 125 backfill must not change any current owner. The
// database is put back in the pre-migration shape (no reg_seq), rows are seeded
// with ties on updated_at where the earlier registrant holds the larger id plus
// untied pairs, the owner per token is computed with the OLD rule, and the
// migration's own SQL is applied.
// Kills: a backfill that orders differently from (updated_at, id).
func TestTask4031_RegSeqBackfillKeepsEveryOwner(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	pg := s.dialect.Driver() == DriverPostgres
	a, b := notifyUser(t, s, "bf-a@example.com"), notifyUser(t, s, "bf-b@example.com")
	t0, t1 := ago(3*time.Hour), ago(2*time.Hour)
	type row struct {
		user, token, id, at string
	}
	// T1, T3: tied updated_at; T2, T4: untied (the larger id is on the older row in T4).
	seed := []row{
		{a.ID, "T1", "zz-a-t1", t0}, {b.ID, "T1", "aa-b-t1", t0}, // tie, earlier registrant a has larger id -> a
		{a.ID, "T2", "aa-a-t2", t0}, {b.ID, "T2", "zz-b-t2", t1}, // untied -> b
		{a.ID, "T3", "aa-a-t3", t0}, {b.ID, "T3", "zz-b-t3", t0}, // tie -> b
		{a.ID, "T4", "zz-a-t4", t1}, {b.ID, "T4", "aa-b-t4", t0}, // untied, older row has the smaller id -> a
	}
	for _, r := range seed {
		d, _ := deviceAt(t, s, r.user, r.token, r.at, time.Hour)
		s.mustExec(t, `UPDATE push_devices SET id = ?, created_at = ?, updated_at = ? WHERE id = ?`, r.id, r.at, r.at, d)
	}

	// Pre-migration shape.
	if pg {
		s.mustExec(t, `DROP INDEX IF EXISTS idx_push_devices_reg_seq`)
		s.mustExec(t, `ALTER TABLE push_devices DROP COLUMN reg_seq`)
		s.mustExec(t, `DROP SEQUENCE push_devices_reg_seq`)
	} else {
		s.mustExec(t, `DROP INDEX idx_push_devices_reg_seq`)
		s.mustExec(t, `ALTER TABLE push_devices DROP COLUMN reg_seq`)
	}
	oldOwners := func() map[string]string {
		rows, err := s.db.Query(`
			SELECT d.token, d.user_id FROM push_devices d
			WHERE NOT EXISTS (SELECT 1 FROM push_devices d2 WHERE d2.token = d.token AND d2.user_id <> d.user_id
				AND (d2.updated_at > d.updated_at OR (d2.updated_at = d.updated_at AND d2.id > d.id)))`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		m := map[string]string{}
		for rows.Next() {
			var tok, u string
			if err := rows.Scan(&tok, &u); err != nil {
				t.Fatal(err)
			}
			m[tok] = u
		}
		return m
	}
	want := oldOwners()
	if want["T1"] != a.ID || want["T2"] != b.ID || want["T3"] != b.ID || want["T4"] != a.ID || len(want) != 4 {
		t.Fatalf("old-rule owners = %v; seed does not exercise the ties as intended", want)
	}

	if pg {
		data, err := pgMigrationsFS.ReadFile("pgmigrations/125_push_devices_reg_seq.sql")
		if err != nil {
			t.Fatal(err)
		}
		if err := applyPostgresMigration(s.db, "125_push_devices_reg_seq_task4031_test.sql", string(data)); err != nil {
			t.Fatal(err)
		}
	} else {
		data, err := migrationsFS.ReadFile("migrations/157_push_devices_reg_seq.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(string(data)); err != nil {
			t.Fatal(err)
		}
	}

	got := map[string]string{}
	for _, u := range []string{a.ID, b.ID} {
		ds, err := s.LivePushDevices(u, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			if prev, dup := got[d.Token]; dup {
				t.Fatalf("token %s owned by two users (%s, %s)", d.Token, prev, u)
			}
			got[d.Token] = u
		}
	}
	keys := func(m map[string]string) []string {
		var k []string
		for t, u := range m {
			k = append(k, t+"="+u)
		}
		sort.Strings(k)
		return k
	}
	if fmt.Sprint(keys(got)) != fmt.Sprint(keys(want)) {
		t.Fatalf("owners after the migration = %v, want the old rule's %v", keys(got), keys(want))
	}
	// A register after the migration still outranks every backfilled row.
	sid, _ := pushSession(t, s, a.ID)
	register(t, s, a.ID, sid, "T2")
	if !liveHas(t, s, a.ID, "T2") || liveHas(t, s, b.ID, "T2") {
		t.Fatal("a register after the backfill does not take the token")
	}
}
