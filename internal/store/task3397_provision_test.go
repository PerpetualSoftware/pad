package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3397 (U8b): the provisioning transaction's lock table, pinned on both
// dialects (run with PAD_TEST_POSTGRES_URL for Postgres, where locks 3-5 are
// real; on SQLite BEGIN IMMEDIATE holds the write lock for the whole
// transaction, so every case below holds there too).

type task3397Fix struct {
	s      *Store
	ws     *models.Workspace
	owner  *models.User
	dest   *models.Collection
	people *models.Collection
	target *models.Item
	req    ProvisionRequest
}

func task3397Fixture(t *testing.T) task3397Fix {
	t.Helper()
	s := testStore(t)
	s.SetAppAPIAudience("https://pad.example/api/app/v1")
	ws := createTestWorkspace(t, s, "Provision")
	owner := createTestUser(t, s, "owner3397@example.com", "Owner", "correct-horse-battery")
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	dest, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Dest", Slug: "dest"})
	if err != nil {
		t.Fatal(err)
	}
	people, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "People", Slug: "people"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.CreateItem(ws.ID, people.ID, models.ItemCreate{Title: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://portal.example"
	p, err := s.ReservePendingInstall(ws.ID, owner.ID, origin)
	if err != nil {
		t.Fatal(err)
	}
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := s.FinishPendingInstall(p.ID, sha, "{}"); err != nil {
		t.Fatal(err)
	}
	return task3397Fix{s: s, ws: ws, owner: owner, dest: dest, people: people, target: target, req: ProvisionRequest{
		PendingID: p.ID, WorkspaceID: ws.ID, OwnerID: owner.ID, Origin: origin,
		ManifestSHA256: sha, ManifestVersion: "1.0.0", ManifestJSON: "{}", AppTitle: "Portal",
		ServiceAccess: "write", DelegatedAccess: "read", SourcePack: origin + "@1.0.0", DigestsJSON: "{}",
	}}
}

func (f task3397Fix) derived(digest string) *ProvisionDerived {
	return &ProvisionDerived{
		Collections: []ProvisionCollection{{Key: "t", Slug: "portal-t", Name: "Tickets", Schema: `{"fields":[]}`}},
		Artifacts: []ProvisionArtifact{{Key: "a", CollectionID: f.dest.ID, Title: "Artifact",
			Fields: map[string]any{"owner": f.target.ID}, RawSHA256: "raw", NormalizedSHA256: digest}},
	}
}

func TestTask3397_ProvisionHappyPath(t *testing.T) {
	f := task3397Fixture(t)
	res, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) { return f.derived("n1"), nil })
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.InstallID == "" || res.InstallCode == "" || len(res.Items) != 1 {
		t.Fatalf("result %+v", res)
	}
	var n int
	if err := f.s.db.QueryRow(f.s.q(`SELECT COUNT(*) FROM app_install_pending WHERE id = ?`), f.req.PendingID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("pending left: %d (%v)", n, err)
	}
}

// Lock 4: the caller must still be an owner inside the transaction.
func TestTask3397_ProvisionRefusesADemotedOwner(t *testing.T) {
	f := task3397Fixture(t)
	other := createTestUser(t, f.s, "other3397@example.com", "Other", "correct-horse-battery")
	if err := f.s.AddWorkspaceMember(f.ws.ID, other.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.UpdateWorkspaceMemberRole(f.ws.ID, f.owner.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) { called = true; return f.derived("n1"), nil })
	if !errors.Is(err, ErrNotWorkspaceOwner) {
		t.Fatalf("got %v, want ErrNotWorkspaceOwner", err)
	}
	if called {
		t.Error("the derivation ran for a caller who is not an owner")
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM app_installs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d installs written (%v)", n, err)
	}
}

// heldDerive returns a derivation that, on its Nth call, signals it is inside
// the transaction and waits for release.
func heldDerive(f task3397Fix, holdOnCall int, inside chan<- struct{}, release <-chan struct{}) ProvisionDeriveFunc {
	calls := 0
	return func(Queryer) (*ProvisionDerived, error) {
		calls++
		if calls == holdOnCall {
			close(inside)
			<-release
		}
		return f.derived("n1"), nil
	}
}

// assertBlockedUntilRelease runs write concurrently with a provisioning held
// inside its transaction: write must not finish while it is held, and must
// finish once it commits.
func assertBlockedUntilRelease(t *testing.T, f task3397Fix, holdOnCall int, write func() error) {
	t.Helper()
	inside, release := make(chan struct{}), make(chan struct{})
	provDone := make(chan error, 1)
	go func() {
		_, err := f.s.ProvisionAppInstall(f.req, heldDerive(f, holdOnCall, inside, release))
		provDone <- err
	}()
	select {
	case <-inside:
	case err := <-provDone:
		t.Fatalf("provisioning ended before the hold: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("provisioning never reached the derivation")
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- write() }()
	select {
	case err := <-writeDone:
		close(release)
		<-provDone
		t.Fatalf("the write finished while provisioning held its locks (err %v)", err)
	case <-time.After(700 * time.Millisecond):
	}
	close(release)
	if err := <-provDone; err != nil {
		t.Fatalf("provision: %v", err)
	}
	select {
	case <-writeDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the write never finished after provisioning committed")
	}
}

// Lock 3: the collections rows are held for the derivation's consistent
// read, so an archive (which skips the workspace lock) waits for the commit.
func TestTask3397_CollectionArchiveWaitsForProvisioning(t *testing.T) {
	f := task3397Fixture(t)
	assertBlockedUntilRelease(t, f, 1, func() error { return f.s.DeleteCollection(f.dest.ID, "") })
}

// Condition (b) of the lead ruling: the two deadlock scenarios codex round 4
// found must not deadlock. Each replays the other writer's lock order in a
// raw transaction around a provisioning, and fails on 40P01 in either one. A
// future lock that recreates either cycle turns these red (both were
// reproduced red against the round-3 lock set).
func task3397NoDeadlock(t *testing.T, f task3397Fix, first, second func(tx *sql.Tx) error) {
	t.Helper()
	w, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := first(w); err != nil {
		_ = w.Rollback()
		t.Fatalf("writer step 1: %v", err)
	}
	provDone := make(chan error, 1)
	go func() {
		_, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) { return f.derived("n1"), nil })
		provDone <- err
	}()
	time.Sleep(700 * time.Millisecond) // provisioning is mid-transaction (or, on SQLite, waiting on BEGIN)
	if err := second(w); err != nil {
		_ = w.Rollback()
		<-provDone
		t.Fatalf("writer step 2 (a deadlock victim?): %v", err)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	select {
	case err := <-provDone:
		if err != nil && strings.Contains(err.Error(), "40P01") {
			t.Fatalf("provisioning was a deadlock victim: %v", err)
		}
		if err != nil && !errors.Is(err, ErrNotWorkspaceOwner) {
			t.Fatalf("provisioning: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("provisioning never finished")
	}
}

// Account deletion of the confirming owner is a real cycle (codex round 5):
// its bot scan reads what provisioning writes. Lock 0 serializes the two
// whole. Provisioning first: the deletion waits, then finds and purges the
// new bot. Neither order may deadlock (the users-row / pending-row cycle).
func TestTask3397_AccountDeletionAfterProvisioning(t *testing.T) {
	f := task3397Fixture(t)
	assertBlockedUntilRelease(t, f, 1, func() error {
		_, err := f.s.DeleteAccountAtomicReport(f.owner.ID)
		return err
	})
	var bots int
	if err := f.s.db.QueryRow(f.s.q(`SELECT COUNT(*) FROM users WHERE kind = 'app'`)).Scan(&bots); err != nil {
		t.Fatal(err)
	}
	if bots != 0 {
		t.Fatalf("%d app bots survived the owner's account deletion", bots)
	}
}

// Deletion first: provisioning waits behind the deletion's user lock, then
// finds the pending record gone with the user, and refuses cleanly.
func TestTask3397_ProvisioningAfterAccountDeletion(t *testing.T) {
	f := task3397Fixture(t)
	locked, release := make(chan struct{}), make(chan struct{})
	deleteAccountAfterUserLockHook = func() {
		deleteAccountAfterUserLockHook = nil
		close(locked)
		<-release
	}
	t.Cleanup(func() { deleteAccountAfterUserLockHook = nil })
	delDone := make(chan error, 1)
	go func() {
		_, err := f.s.DeleteAccountAtomicReport(f.owner.ID)
		delDone <- err
	}()
	<-locked
	provDone := make(chan error, 1)
	go func() {
		_, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) { return f.derived("n1"), nil })
		provDone <- err
	}()
	select {
	case err := <-provDone:
		close(release)
		t.Fatalf("provisioning finished while the deletion held the user (err %v)", err)
	case <-time.After(700 * time.Millisecond):
	}
	close(release)
	if err := <-delDone; err != nil {
		t.Fatalf("account deletion: %v", err)
	}
	select {
	case err := <-provDone:
		if !errors.Is(err, ErrPendingNotStaged) && !errors.Is(err, ErrNotWorkspaceOwner) {
			t.Fatalf("provisioning: %v, want a clean refusal after the deletion", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("provisioning never finished")
	}
}

// Member removal (RemoveWorkspaceMemberAndRevokeGrants): it deletes the
// member's grants, then their membership.
func TestTask3397_MemberRemovalDoesNotDeadlock(t *testing.T) {
	f := task3397Fixture(t)
	other := createTestUser(t, f.s, "third3397@example.com", "Third", "correct-horse-battery")
	if err := f.s.AddWorkspaceMember(f.ws.ID, other.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreateCollectionGrant(f.ws.ID, f.people.ID, f.owner.ID, "view", other.ID); err != nil {
		t.Fatal(err)
	}
	task3397NoDeadlock(t, f,
		func(tx *sql.Tx) error {
			_, err := tx.Exec(f.s.q(`DELETE FROM collection_grants WHERE workspace_id = ? AND user_id = ?`), f.ws.ID, f.owner.ID)
			return err
		},
		func(tx *sql.Tx) error {
			_, err := tx.Exec(f.s.q(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`), f.ws.ID, f.owner.ID)
			return err
		})
}
