package store

import (
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3099, the lock order on Postgres. DeleteAccountAtomic takes the users-
// row lock a limited workspace mint takes (enforceUserLimitTx) as its FIRST
// statement and reads the owned set under it. These tests hold a real mint
// open mid-transaction at each of its two points and drive the deletion
// against it. SQLite is not driven: BEGIN IMMEDIATE serialises every writer,
// so the interleavings below cannot arise there.

func pgStoreFor3099(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; Postgres lock-order test skipped")
	}
	return testStorePostgres(t, url)
}

func deleteAsync(s *Store, userID string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- s.DeleteAccountAtomic(userID) }()
	return done
}

func liveOwned(t *testing.T, s *Store, wsID string) bool {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(s.q(`SELECT COUNT(*) FROM workspaces WHERE id = ? AND deleted_at IS NULL`), wsID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// The mint holds the owner lock. The deletion must WAIT for it, and then see
// the committed workspace and delete it. Without the lock the deletion reads
// its set before the mint commits, and the workspace survives live.
func TestDeleteAccountAtomic_WaitsForAMintHoldingTheOwnerLock(t *testing.T) {
	s := pgStoreFor3099(t)
	u, err := s.CreateUser(models.UserCreate{Email: "m1@example.com", Name: "M", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ws, err := s.createWorkspaceQ(tx, models.WorkspaceCreate{Name: "Minting", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.enforceUserLimitTx(tx, u.ID, "workspaces", 1); err != nil {
		t.Fatal(err)
	}

	done := deleteAsync(s, u.ID)
	select {
	case err := <-done:
		t.Fatalf("deletion finished while the mint held the owner lock (err=%v): it read its set without waiting", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("deletion: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deletion never finished after the mint committed")
	}
	if liveOwned(t, s, ws.ID) {
		t.Fatal("the workspace the mint committed survived the deletion")
	}
}

// The mint has inserted its row but not yet taken the lock. The deletion must
// NOT wait on it (the mint holds nothing on users: no deadlock), and the mint
// must then fail, finding no owner, so no workspace outlives its owner.
func TestDeleteAccountAtomic_DoesNotWaitOnAMintBeforeItsLock(t *testing.T) {
	s := pgStoreFor3099(t)
	u, err := s.CreateUser(models.UserCreate{Email: "m2@example.com", Name: "M", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ws, err := s.createWorkspaceQ(tx, models.WorkspaceCreate{Name: "Minting", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-deleteAsync(s, u.ID):
		if err != nil {
			t.Fatalf("deletion: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deletion blocked on a mint that has not taken the owner lock")
	}
	if err := s.enforceUserLimitTx(tx, u.ID, "workspaces", 1); err == nil {
		t.Fatal("the mint took the owner lock of a deleted user")
	}
	_ = tx.Rollback()
	if liveOwned(t, s, ws.ID) {
		t.Fatal("a workspace outlived its owner")
	}
}
