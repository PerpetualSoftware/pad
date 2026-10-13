package appstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3653 part B: an app upload is charged against the workspace owner's
// storage_bytes limit, as a human upload is (TASK-3619), on top of the
// install's own caps. Cloud only (Options.PlanLimit), like the item cap.

func newCappedUploadFixture(t *testing.T, planLimit bool, limit string) uploadFixture {
	t.Helper()
	blobDir := t.TempDir()
	blobs, err := attachments.NewFSStore(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	inflight := &attachments.InFlight{}
	f := newAppFixture(t, Options{Blobs: blobs, InFlight: inflight, PlanLimit: planLimit})
	if _, err := f.s.SetUserPlan(f.owner.ID, store.PlanWrite{Plan: "pro", Source: store.PlanSourceManual, Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetUserPlanOverrides(f.owner.ID, `{"storage_bytes":`+limit+`}`); err != nil {
		t.Fatal(err)
	}
	mine, err := f.a.CreateItem(context.Background(), f.spec, f.companion.ID, AppItemCreate{Title: "Ticket with files"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	return uploadFixture{appFixture: f, blobDir: blobDir, blobs: blobs, inflight: inflight, mine: mine}
}

// blobFiles counts the canonical blobs on disk, ignoring the staging area.
func blobFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.Contains(p, "staging") && !strings.HasPrefix(d.Name(), ".") {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBUG3653_AppUploadPastTheStorageCapIsRefused(t *testing.T) {
	ctx := context.Background()
	f := newCappedUploadFixture(t, true, "20")
	if _, err := f.upload(ctx, f.spec, f.mine.ID, []byte(strings.Repeat("a", 15)), "a.txt"); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	before := blobFiles(t, f.blobDir)
	_, err := f.upload(ctx, f.spec, f.mine.ID, []byte(strings.Repeat("b", 15)), "b.txt")
	if !errors.Is(err, store.ErrAppStorageLimit) {
		t.Fatalf("an app upload past the storage cap: err = %v, want ErrAppStorageLimit", err)
	}
	if strings.ContainsAny(err.Error(), "0123456789") {
		t.Errorf("the refusal carries a number: %q", err)
	}
	if after := blobFiles(t, f.blobDir); after != before {
		t.Errorf("blobs on disk: %d before the refused upload, %d after", before, after)
	}

	// Self-hosted (PlanLimit off): the same upload lands.
	g := newCappedUploadFixture(t, false, "20")
	if _, err := g.upload(ctx, g.spec, g.mine.ID, []byte(strings.Repeat("a", 15)), "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.upload(ctx, g.spec, g.mine.ID, []byte(strings.Repeat("b", 15)), "b.txt"); err != nil {
		t.Fatalf("self-hosted upload past the cap: %v", err)
	}
}

// Admission is advisory; the row insert charges the cap. An upload admitted
// while there was room is refused at its insert when another upload filled
// the cap in between.
func TestBUG3653_AppUploadInsertChargesTheCapAuthoritatively(t *testing.T) {
	ctx := context.Background()
	f := newCappedUploadFixture(t, true, "20")
	body := []byte(strings.Repeat("p", 15))
	pending, err := f.a.StageUpload(ctx, f.spec, f.mine.ID, AppUpload{Body: bytes.NewReader(body), DeclaredSize: int64(len(body)), Filename: "p.txt"}, f.actor)
	if err != nil {
		t.Fatalf("admission with room: %v", err)
	}
	defer pending.Close()
	if _, err := f.upload(ctx, f.spec, f.mine.ID, []byte(strings.Repeat("q", 10)), "q.txt"); err != nil {
		t.Fatalf("the upload that fills the cap: %v", err)
	}
	if _, err := pending.Insert(ctx); !errors.Is(err, store.ErrAppStorageLimit) {
		t.Fatalf("insert past the cap: err = %v, want ErrAppStorageLimit", err)
	}
}
