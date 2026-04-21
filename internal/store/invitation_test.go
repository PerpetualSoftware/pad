package store

import (
	"testing"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// TestCreateInvitation_SetsExpiresAt verifies new invitations get an expires_at
// ~InvitationTTL in the future, round-tripped through store hydration.
func TestCreateInvitation_SetsExpiresAt(t *testing.T) {
	s := testStore(t)

	// Minimal workspace + user fixtures
	u, err := s.CreateUser(models.UserCreate{
		Email:    "inviter@example.com",
		Username: "inviter",
		Name:     "Inviter",
		Password: "correcthorsebattery",
		Role:     "admin",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "WS", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	before := time.Now().UTC()
	inv, err := s.CreateInvitation(ws.ID, "invitee@example.com", "editor", u.ID)
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	after := time.Now().UTC()

	if inv.ExpiresAt == nil {
		t.Fatal("ExpiresAt is nil on a freshly-created invitation")
	}
	// Expect expires_at ≈ now + InvitationTTL (±1s margin).
	wantLow := before.Add(InvitationTTL - time.Second)
	wantHigh := after.Add(InvitationTTL + time.Second)
	if inv.ExpiresAt.Before(wantLow) || inv.ExpiresAt.After(wantHigh) {
		t.Fatalf("ExpiresAt = %v, expected within [%v, %v]", inv.ExpiresAt, wantLow, wantHigh)
	}
	if inv.IsExpired() {
		t.Fatal("fresh invitation marked expired")
	}
}

// TestMigration044_BackfillParsesCorrectly ensures the SQLite/Postgres backfill
// writes expires_at in a shape that parseTime can round-trip. Codex flagged
// the first cut of this migration because the default datetime()/::TEXT casts
// produce space-separated output that parseTime silently rejects — that would
// mark every pre-existing invitation as already expired right after upgrade.
//
// The test creates a fresh store (which auto-runs migrations, including 044's
// backfill against any rows with NULL expires_at), inserts a legacy-style row
// with NULL expires_at via raw SQL to simulate a pre-migration record, runs
// the backfill UPDATE manually (since it only runs during migration time for
// rows that exist AT migration time), and re-hydrates via the usual store API.
func TestMigration044_BackfillProducesRFC3339(t *testing.T) {
	s := testStore(t)

	u, err := s.CreateUser(models.UserCreate{
		Email: "a@b.com", Username: "a", Name: "A", Password: "correcthorsebattery", Role: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "W2", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a pre-migration row: insert invitation directly with expires_at NULL.
	id := newID()
	ts := now()
	_, err = s.db.Exec(s.q(`
		INSERT INTO workspace_invitations (id, workspace_id, email, role, invited_by, code, code_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
	`), id, ws.ID, "legacy@example.com", "editor", u.ID, id, "deadbeef", ts)
	if err != nil {
		t.Fatalf("insert legacy invitation: %v", err)
	}

	// Apply the same backfill expression the migration uses. We can't re-run
	// the full migration (already applied by New()), so we mirror it here.
	switch s.dialect.(type) {
	case *sqliteDialect:
		_, err = s.db.Exec(`
			UPDATE workspace_invitations
			SET expires_at = strftime('%Y-%m-%dT%H:%M:%SZ', created_at, '+14 days')
			WHERE expires_at IS NULL
		`)
	case *postgresDialect:
		_, err = s.db.Exec(`
			UPDATE workspace_invitations
			SET expires_at = to_char(
				created_at::timestamp + INTERVAL '14 days',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'
			)
			WHERE expires_at IS NULL
		`)
	}
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}

	inv, err := s.GetInvitation(id)
	if err != nil {
		t.Fatalf("get invitation: %v", err)
	}
	if inv == nil {
		t.Fatal("legacy invitation disappeared")
	}
	if inv.ExpiresAt == nil {
		t.Fatal("ExpiresAt still nil after backfill")
	}
	if inv.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt is zero Time — parseTime failed to parse the backfilled value")
	}
	if inv.IsExpired() {
		t.Fatalf("legacy invitation marked expired after backfill (ExpiresAt=%v)", inv.ExpiresAt)
	}
	// expires_at should be ~InvitationTTL after created_at (allow 2s drift for wall-clock jitter).
	diff := inv.ExpiresAt.Sub(inv.CreatedAt)
	if diff < InvitationTTL-2*time.Second || diff > InvitationTTL+2*time.Second {
		t.Fatalf("backfill delta = %v, expected ~%v", diff, InvitationTTL)
	}
}
