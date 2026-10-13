package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3653: a cross-workspace copy charges the attachment rows it clones
// against the destination owner's storage_bytes limit.

// copyStorageFixture is a source workspace whose item references one
// original of origBytes with one thumbnail variant of variantBytes, and a
// sibling destination workspace with a collection, both owned by one user.
type copyStorageFixture struct {
	*storageFixture
	dst    *storageFixture
	dstCol string
	srcRef string
}

func newCopyStorageFixture(t *testing.T, plan string, origBytes, variantBytes int64) *copyStorageFixture {
	t.Helper()
	f := newStorageFixture(t, plan)
	orig := &models.Attachment{ID: newID(), WorkspaceID: f.wsID, ItemID: &f.itemID, StorageKey: "fs:" + newID(), ContentHash: newID(),
		MimeType: "image/png", SizeBytes: origBytes, Filename: "pic.png"}
	if err := f.s.CreateAttachment(orig); err != nil {
		t.Fatal(err)
	}
	variant := models.AttachmentVariantThumbSm
	thumb := &models.Attachment{ID: newID(), WorkspaceID: f.wsID, ItemID: &f.itemID, StorageKey: "fs:" + newID(), ContentHash: newID(),
		MimeType: "image/png", SizeBytes: variantBytes, Filename: "pic.png", ParentID: &orig.ID, Variant: &variant}
	if err := f.s.CreateAttachment(thumb); err != nil {
		t.Fatal(err)
	}
	content := "![pic](pad-attachment:" + orig.ID + ")"
	if _, err := f.s.UpdateItem(f.itemID, models.ItemUpdate{Content: &content}); err != nil {
		t.Fatal(err)
	}
	dst := f.sibling(t)
	col, err := f.s.CreateCollection(dst.wsID, models.CollectionCreate{Name: "Tasks"})
	if err != nil {
		t.Fatal(err)
	}
	return &copyStorageFixture{storageFixture: f, dst: dst, dstCol: col.ID, srcRef: f.itemID}
}

func (f *copyStorageFixture) copy(enforce bool) error {
	_, err := f.s.CopyItemAcrossWorkspaces(CrossWorkspaceCopyRequest{
		SourceItemID:        f.srcRef,
		TargetWorkspaceID:   f.dst.wsID,
		TargetCollectionID:  f.dstCol,
		Actor:               f.owner.ID,
		EnforceStorageLimit: enforce,
	})
	return err
}

func (f *copyStorageFixture) dstRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(f.s.q(`SELECT COUNT(*) FROM attachments WHERE workspace_id = ? AND deleted_at IS NULL`), f.dst.wsID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Free is per workspace, so the destination starts empty. With a limit of
// 120 the original alone (100) would fit; with its 50-byte thumbnail the copy
// adds 150 and is refused, which proves the variant is charged. At 150 it
// lands exactly on the limit. Unenforced (self-hosted) it always lands.
func TestBUG3653_CopyChargesEveryClonedRowIncludingVariants(t *testing.T) {
	f := newCopyStorageFixture(t, "free", 100, 50)
	if err := f.s.SetUserPlanOverrides(f.owner.ID, `{"storage_bytes":120}`); err != nil {
		t.Fatal(err)
	}
	ple := planLimitErr(t, f.copy(true))
	if ple.Result.Requested != 150 || ple.Result.Current != 0 || ple.Result.Limit != 120 || ple.Result.Feature != "storage_bytes" {
		t.Fatalf("refusal = %+v, want requested 150, current 0, limit 120", ple.Result)
	}
	if n := f.dstRows(t); n != 0 {
		t.Fatalf("refused copy left %d rows in the destination", n)
	}
	if err := f.copy(false); err != nil {
		t.Fatalf("unenforced copy: %v", err)
	}
	if n := f.dstRows(t); n != 2 {
		t.Fatalf("unenforced copy cloned %d rows, want 2", n)
	}

	g := newCopyStorageFixture(t, "free", 100, 50)
	if err := g.s.SetUserPlanOverrides(g.owner.ID, `{"storage_bytes":150}`); err != nil {
		t.Fatal(err)
	}
	if err := g.copy(true); err != nil {
		t.Fatalf("copy landing exactly on the limit: %v", err)
	}
}

// Pro pools the account: a copy into one workspace and an upload into the
// other, with room for only one, never both pass. Repeated in rounds because
// on Postgres the race is real and only the storage lock serialises them.
func TestBUG3653_ConcurrentCopyAndUploadShareThePool(t *testing.T) {
	f := newCopyStorageFixture(t, "pro", 600, 0)
	for round := 0; round < 25; round++ {
		used, err := f.s.accountStorageUsageQ(f.s.db, f.owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Room for exactly one of the two 600-byte writes.
		if err := f.s.SetUserPlanOverrides(f.owner.ID, fmt.Sprintf(`{"storage_bytes":%d}`, used+1000)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errs[0] = f.copy(true)
		}()
		go func() {
			defer wg.Done()
			<-start
			errs[1] = f.add(600, false, WithPlanLimit())
		}()
		close(start)
		wg.Wait()
		refused := 0
		for _, err := range errs {
			var ple *PlanLimitError
			if errors.As(err, &ple) {
				refused++
			} else if err != nil {
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
		}
		if refused != 1 {
			t.Fatalf("round %d: refused = %d of 2, want exactly 1 (errs %v)", round, refused, errs)
		}
	}
}

// An ownership change between the destination's owner read and the check is
// detected, and the copy retried in a fresh transaction, as an upload is.
func TestBUG3653_CopyRetriesAnOwnershipChangeMidCheck(t *testing.T) {
	f := newCopyStorageFixture(t, "pro", 10, 0)
	heir, err := f.s.CreateUser(models.UserCreate{Email: "heir-" + newID()[:8] + "@example.com", Name: "Heir", Password: "correct-horse-battery-staple", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	var calls, moveOn int
	testHookAfterStorageOwnerRead = func(tx *sql.Tx, ws string) {
		calls++
		if ws == f.dst.wsID && calls <= moveOn {
			if _, err := tx.Exec(f.s.q(`UPDATE workspaces SET owner_id = ? WHERE id = ?`), heir.ID, f.dst.wsID); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { testHookAfterStorageOwnerRead = nil })

	calls, moveOn = 0, 1
	if err := f.copy(true); err != nil {
		t.Fatalf("a move on the first attempt only: %v", err)
	}
	if calls != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry)", calls)
	}

	calls, moveOn = 0, 99
	if err := f.copy(true); !errors.Is(err, errStorageOwnerMoved) {
		t.Fatalf("a move on every attempt: err = %v, want errStorageOwnerMoved", err)
	}
	if calls != 3 {
		t.Fatalf("attempts = %d, want 3", calls)
	}
}

// TASK-3779: the race above never overlapped the two writes, so it passed
// with the storage lock removed. Here the copy is held after its check passes
// and before it inserts, with room for only one of the two 600-byte writes,
// and the upload starts inside that window. With the lock the upload waits for
// the copy to commit and is refused; without it both commit past the cap.
func TestTASK3779_UploadWaitsForAHeldCopyAndIsRefused(t *testing.T) {
	f := newCopyStorageFixture(t, "pro", 600, 0)
	if f.s.dialect.Driver() != DriverPostgres {
		// The advisory lock under test is Postgres-only. On SQLite the
		// upload's BEGIN IMMEDIATE waits behind the held copy's writer lock
		// before it can reach the lock this test watches.
		t.Skip("the storage advisory lock is Postgres-only")
	}
	used, err := f.s.accountStorageUsageQ(f.s.db, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetUserPlanOverrides(f.owner.ID, fmt.Sprintf(`{"storage_bytes":%d}`, used+1000)); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseCopy := func() { releaseOnce.Do(func() { close(release) }) }
	testHookCopyAfterStorageCheck = func() {
		close(held)
		<-release
	}
	copyErr, uploadErr := make(chan error, 1), make(chan error, 1)
	copyDone, uploadDone := make(chan struct{}), make(chan struct{})
	uploadStarted := false
	// Failure paths must not leave the copy parked holding its locks, or a
	// goroutine running past the test.
	t.Cleanup(func() {
		releaseCopy()
		<-copyDone
		if uploadStarted {
			<-uploadDone
		}
		testHookCopyAfterStorageCheck = nil
	})

	go func() { defer close(copyDone); copyErr <- f.copy(true) }()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the copy never reached its storage check")
	}
	uploadStarted = true
	go func() { defer close(uploadDone); uploadErr <- f.add(600, false, WithPlanLimit()) }()

	// The upload must be seen WAITING on an advisory lock in this database,
	// in Postgres's own lock table, while the copy is held: the proof that
	// the lock serialises them, not merely that the upload was slow. Without
	// the lock no waiter ever appears and the upload commits past the cap.
	waiting := false
	deadline := time.Now().Add(10 * time.Second)
	for !waiting && time.Now().Before(deadline) {
		select {
		case err := <-uploadErr:
			t.Fatalf("the upload finished while the copy was held between its check and its commit (err %v): the storage lock does not serialise them", err)
		case <-time.After(20 * time.Millisecond):
		}
		var n int
		if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		waiting = n > 0
	}
	if !waiting {
		t.Fatal("the upload never waited on an advisory lock while the copy was held")
	}
	releaseCopy()
	if err := <-copyErr; err != nil {
		t.Fatalf("the held copy: %v", err)
	}
	var ple *PlanLimitError
	if err := <-uploadErr; !errors.As(err, &ple) {
		t.Fatalf("the upload after the copy committed: err = %v, want *PlanLimitError", err)
	}
	if usage, _ := f.s.accountStorageUsageQ(f.s.db, f.owner.ID); usage > used+1000 {
		t.Fatalf("usage %d is past the limit %d", usage, used+1000)
	}
}
