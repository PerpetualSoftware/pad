package store

import (
	"errors"
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
		ResolvedItemIDs: []string{f.target.ID},
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
	// On Postgres the first, read-only derivation runs before the role check
	// (it names the items to lock first; see the lock order). Nothing is
	// written either way.
	_, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) { return f.derived("n1"), nil })
	if !errors.Is(err, ErrNotWorkspaceOwner) {
		t.Fatalf("got %v, want ErrNotWorkspaceOwner", err)
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

// Lock 3: a collection archive skips the workspace lock, so only the FOR
// SHARE on every collections row holds it off (codex round 2).
func TestTask3397_CollectionArchiveWaitsForProvisioning(t *testing.T) {
	f := task3397Fixture(t)
	assertBlockedUntilRelease(t, f, 1, func() error { return f.s.DeleteCollection(f.dest.ID, "") })
}

// Lock 5: a resolved relation target is held FOR SHARE. The write is a raw
// UPDATE that takes no workspace lock, so on Postgres only lock 5 can hold it
// off. The hold is on the SECOND derivation, which runs after lock 5 (on
// SQLite there is one derivation and the write lock covers it).
func TestTask3397_ResolvedItemWaitsForProvisioning(t *testing.T) {
	f := task3397Fixture(t)
	hold := 1
	if f.s.dialect.Driver() == DriverPostgres {
		hold = 2
	}
	assertBlockedUntilRelease(t, f, hold, func() error {
		_, err := f.s.db.Exec(f.s.q(`UPDATE items SET title = 'Renamed' WHERE id = ?`), f.target.ID)
		return err
	})
}

// Codex round 3: the visibility check reads the caller's grants, so a grant
// revocation must wait too. The hold is on the second derivation, after the
// visibility rows are locked (Postgres); SQLite's write lock covers it.
func TestTask3397_GrantRevocationWaitsForProvisioning(t *testing.T) {
	f := task3397Fixture(t)
	g, err := f.s.CreateCollectionGrant(f.ws.ID, f.people.ID, f.owner.ID, "view", f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	hold := 1
	if f.s.dialect.Driver() == DriverPostgres {
		hold = 2
	}
	assertBlockedUntilRelease(t, f, hold, func() error {
		_, err := f.s.db.Exec(f.s.q(`DELETE FROM collection_grants WHERE id = ?`), g.ID)
		return err
	})
}

// Codex round 3: account deletion updates the caller's items, then deletes
// their membership. Provisioning takes the resolved items BEFORE the
// membership, the same order, so the two queue instead of deadlocking: the
// deletion commits first and provisioning then finds no owner.
func TestTask3397_AccountDeletionOrderDoesNotDeadlock(t *testing.T) {
	f := task3397Fixture(t)
	other := createTestUser(t, f.s, "second3397@example.com", "Second", "correct-horse-battery")
	if err := f.s.AddWorkspaceMember(f.ws.ID, other.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	del, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Step 1 of the deletion: an UPDATE of the resolved target item.
	if _, err := del.Exec(f.s.q(`UPDATE items SET title = 'Ada (deleted author)' WHERE id = ?`), f.target.ID); err != nil {
		t.Fatal(err)
	}
	provDone := make(chan error, 1)
	go func() {
		_, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) { return f.derived("n1"), nil })
		provDone <- err
	}()
	time.Sleep(700 * time.Millisecond) // provisioning is now waiting on the item (or, on SQLite, on BEGIN)
	// Step 2 of the deletion: the membership.
	if _, err := del.Exec(f.s.q(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`), f.ws.ID, f.owner.ID); err != nil {
		t.Fatalf("the deletion could not take the membership (a deadlock victim?): %v", err)
	}
	if err := del.Commit(); err != nil {
		t.Fatalf("deletion commit: %v", err)
	}
	select {
	case err := <-provDone:
		if !errors.Is(err, ErrNotWorkspaceOwner) {
			t.Fatalf("provisioning: %v, want ErrNotWorkspaceOwner after the deletion committed", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("provisioning never finished")
	}
}

// Postgres re-derives after lock 5 and refuses if the two disagree. SQLite
// derives once: its write lock was already held for the first pass.
func TestTask3397_SecondDerivationMustAgree(t *testing.T) {
	f := task3397Fixture(t)
	calls := 0
	_, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) {
		calls++
		if calls == 1 {
			return f.derived("n1"), nil
		}
		return f.derived("n2"), nil
	})
	if f.s.dialect.Driver() != DriverPostgres {
		if err != nil || calls != 1 {
			t.Fatalf("sqlite: err %v, %d derivations; want success after one", err, calls)
		}
		return
	}
	var pc *ProvisionConflictError
	if !errors.As(err, &pc) || calls != 2 {
		t.Fatalf("got %v after %d derivations, want a conflict after 2", err, calls)
	}
}
