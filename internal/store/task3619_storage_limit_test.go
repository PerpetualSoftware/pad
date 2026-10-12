package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// storageFixture is a workspace owned by a user on the given plan, with one
// live item to bind attachments to.
type storageFixture struct {
	s      *Store
	owner  *models.User
	wsID   string
	itemID string
}

func newStorageFixture(t *testing.T, plan string) *storageFixture {
	t.Helper()
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{Email: "cap-" + newID()[:8] + "@example.com", Name: "Cap Owner", Password: "correct-horse-battery-staple", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUserPlan(owner.ID, PlanWrite{Plan: plan, Source: PlanSourceManual, Force: true}); err != nil {
		t.Fatal(err)
	}
	wsID := newID()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`INSERT INTO workspaces (id, slug, name, settings, owner_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
		wsID, "cap-"+wsID[:8], "Cap", "{}", owner.ID, ts, ts); err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(wsID, models.CollectionCreate{Name: "Tasks"})
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.CreateItem(wsID, coll.ID, models.ItemCreate{Title: "Holds files"})
	if err != nil {
		t.Fatal(err)
	}
	return &storageFixture{s: s, owner: owner, wsID: wsID, itemID: it.ID}
}

// sibling is a second workspace owned by the same user, on the same store.
func (f *storageFixture) sibling(t *testing.T) *storageFixture {
	t.Helper()
	wsID := newID()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := f.s.db.Exec(f.s.q(`INSERT INTO workspaces (id, slug, name, settings, owner_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
		wsID, "cap-"+wsID[:8], "Cap 2", "{}", f.owner.ID, ts, ts); err != nil {
		t.Fatal(err)
	}
	return &storageFixture{s: f.s, owner: f.owner, wsID: wsID}
}

// add inserts an attachment row of size bytes through the limited door.
// bound=false leaves it unattached (the `upload -` shape).
func (f *storageFixture) add(size int64, bound bool, opts ...MintOption) error {
	a := &models.Attachment{ID: newID(), WorkspaceID: f.wsID, StorageKey: "fs:" + newID(), ContentHash: newID(),
		MimeType: "application/octet-stream", SizeBytes: size, Filename: "f.bin"}
	if bound {
		a.ItemID = &f.itemID
	}
	return f.s.CreateAttachmentForLiveItem(a, opts...)
}

func planLimitErr(t *testing.T, err error) *PlanLimitError {
	t.Helper()
	var ple *PlanLimitError
	if !errors.As(err, &ple) {
		t.Fatalf("err = %v, want *PlanLimitError", err)
	}
	return ple
}

// TASK-3619: on Pro the 1 GB cap holds at the insert. Landing exactly on the
// limit is allowed; one byte past it is refused, with the bytes named.
func TestTASK3619_ProStorageCapBothSides(t *testing.T) {
	f := newStorageFixture(t, "pro")
	limit := int64(DefaultProLimits.StorageBytes)
	if err := f.add(limit-100, true, WithPlanLimit()); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	if err := f.add(100, false, WithPlanLimit()); err != nil {
		t.Fatalf("exactly at the cap (unattached): %v", err)
	}
	ple := planLimitErr(t, f.add(1, true, WithPlanLimit()))
	r := ple.Result
	if r.Feature != "storage_bytes" || r.Plan != "pro" || int64(r.Limit) != limit || int64(r.Current) != limit || r.Requested != 1 || r.Allowed {
		t.Fatalf("refusal = %+v", r)
	}
	if r.PayerID != f.owner.ID || r.PayerName != "Cap Owner" {
		t.Fatalf("payer = %q %q, want the owner", r.PayerID, r.PayerName)
	}
	// The unattached door refuses too.
	planLimitErr(t, f.add(1, false, WithPlanLimit()))
	// Nothing was written by the refusals.
	if used, _ := f.s.WorkspaceStorageUsage(f.wsID); used != limit {
		t.Fatalf("used = %d after refusals, want %d", used, limit)
	}
}

// A derived row (a variant) never refuses, and without WithPlanLimit (self-
// hosted callers) nothing does.
func TestTASK3619_VariantsAndUnlimitedCallersNeverRefuse(t *testing.T) {
	f := newStorageFixture(t, "free")
	limit := int64(DefaultFreeLimits.StorageBytes)
	if err := f.add(limit, true, WithPlanLimit()); err != nil {
		t.Fatalf("fill: %v", err)
	}
	parent := newID()
	v := models.AttachmentVariantThumbSm
	variant := &models.Attachment{ID: newID(), WorkspaceID: f.wsID, ItemID: &f.itemID, StorageKey: "fs:" + newID(), ContentHash: newID(),
		MimeType: "image/png", SizeBytes: 5000, Filename: "t.png", ParentID: &parent, Variant: &v}
	if err := f.s.CreateAttachmentForLiveItem(variant, WithPlanLimit()); err != nil {
		t.Fatalf("variant refused: %v", err)
	}
	if err := f.add(10, true); err != nil {
		t.Fatalf("no WithPlanLimit refused: %v", err)
	}
	// Control: the same original WITH the option is refused here.
	planLimitErr(t, f.add(10, true, WithPlanLimit()))
}

// Self-hosted owners have no storage limit.
func TestTASK3619_SelfHostedUnlimited(t *testing.T) {
	f := newStorageFixture(t, "self-hosted")
	if err := f.add(int64(DefaultProLimits.StorageBytes)*3, true, WithPlanLimit()); err != nil {
		t.Fatalf("self-hosted refused: %v", err)
	}
}

// Two uploads that each fit but not together: exactly one lands, whichever
// commits second counts the first (the limit lock).
func TestTASK3619_ConcurrentUploadsCannotBothPassTheCap(t *testing.T) {
	f := newStorageFixture(t, "pro")
	limit := int64(DefaultProLimits.StorageBytes)
	if err := f.add(limit-1000, true, WithPlanLimit()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.add(600, true, WithPlanLimit())
		}(i)
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, err := range errs {
		var ple *PlanLimitError
		switch {
		case err == nil:
			ok++
		case errors.As(err, &ple):
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("ok=%d refused=%d, want 1 and 1", ok, refused)
	}
	if used, _ := f.s.WorkspaceStorageUsage(f.wsID); used > limit {
		t.Fatalf("used %d is past the limit %d", used, limit)
	}
}

// SeedPlanLimits rewrites the old seeded storage values (Pro 10 GB, Free
// 500 MB), and only those.
func TestTASK3619_SeedRewritesOnlyTheOldStorageDefaults(t *testing.T) {
	for _, tc := range []struct{ key, stored, want string }{
		{"plan_limits_pro_storage_bytes", "", "1073741824"},
		{"plan_limits_pro_storage_bytes", "10737418240", "1073741824"},
		{"plan_limits_pro_storage_bytes", "5368709120", "5368709120"}, // an admin's 5 GB survives
		{"plan_limits_pro_storage_bytes", "-1", "-1"},
		{"plan_limits_free_storage_bytes", "", "104857600"},
		{"plan_limits_free_storage_bytes", "524288000", "104857600"},
		{"plan_limits_free_storage_bytes", "262144000", "262144000"}, // an admin's 250 MB survives
	} {
		s := testStore(t)
		if tc.stored != "" {
			if err := s.SetPlatformSetting(tc.key, tc.stored); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SeedPlanLimits(); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetPlatformSetting(tc.key); got != tc.want {
			t.Errorf("%s stored %q: after seed = %q, want %q", tc.key, tc.stored, got, tc.want)
		}
	}
	if DefaultFreeLimits.StorageBytes != 100<<20 {
		t.Errorf("DefaultFreeLimits.StorageBytes = %d, want 100 MB", DefaultFreeLimits.StorageBytes)
	}
}

// Pro is one pool per ACCOUNT: bytes in one owned workspace count against an
// upload into another, and a soft-deleted owned workspace still counts until
// it is purged (TASK-3619, lead rulings).
func TestTASK3619_ProPoolsAcrossOwnedWorkspacesIncludingSoftDeleted(t *testing.T) {
	f := newStorageFixture(t, "pro")
	limit := int64(DefaultProLimits.StorageBytes)
	second := f.sibling(t)
	if err := second.add(limit-10, false, WithPlanLimit()); err != nil {
		t.Fatalf("fill the sibling: %v", err)
	}
	ple := planLimitErr(t, f.add(11, true, WithPlanLimit()))
	if ple.Result.Scope != StorageScopeAccount || int64(ple.Result.Current) != limit-10 {
		t.Fatalf("refusal = %+v, want scope account with the sibling's bytes", ple.Result)
	}
	if err := f.add(10, true, WithPlanLimit()); err != nil {
		t.Fatalf("the remaining 10 bytes: %v", err)
	}
	info, err := f.s.WorkspaceStorageInfo(f.wsID)
	if err != nil || info.Scope != StorageScopeAccount || info.UsedBytes != limit {
		t.Fatalf("info = %+v, %v; want the account total %d", info, err, limit)
	}

	// Soft-delete the sibling (DeleteWorkspace takes the slug): its bytes
	// still count.
	if err := f.s.DeleteWorkspace("cap-" + second.wsID[:8]); err != nil {
		t.Fatal(err)
	}
	planLimitErr(t, f.add(1, true, WithPlanLimit()))
}

// Free stays per workspace: two owned workspaces each hold the full limit.
func TestTASK3619_FreeIsPerWorkspace(t *testing.T) {
	f := newStorageFixture(t, "free")
	limit := int64(DefaultFreeLimits.StorageBytes)
	second := f.sibling(t)
	if err := second.add(limit, false, WithPlanLimit()); err != nil {
		t.Fatal(err)
	}
	if err := f.add(limit, true, WithPlanLimit()); err != nil {
		t.Fatalf("free workspace refused for a sibling's bytes: %v", err)
	}
	info, _ := f.s.WorkspaceStorageInfo(f.wsID)
	if info.Scope != StorageScopeWorkspace || info.UsedBytes != limit {
		t.Fatalf("info = %+v", info)
	}
}

// Two uploads into two DIFFERENT workspaces of one Pro account, each fitting
// alone but not together: the owner-keyed lock makes the second count the
// first. Repeated, because one race can interleave benignly; on Postgres a
// per-workspace lock lets both through in some round (SQLite serialises every
// writer, so there it holds either way).
func TestTASK3619_ConcurrentUploadsAcrossWorkspacesShareThePool(t *testing.T) {
	f := newStorageFixture(t, "pro")
	second := f.sibling(t)
	for round := 0; round < 25; round++ {
		used, err := f.s.accountStorageUsageQ(f.s.db, f.owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Room for exactly one of the two 600-byte uploads.
		if err := f.s.SetUserPlanOverrides(f.owner.ID, fmt.Sprintf(`{"storage_bytes":%d}`, used+1000)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i, fx := range []*storageFixture{f, second} {
			wg.Add(1)
			go func(i int, fx *storageFixture) {
				defer wg.Done()
				<-start
				errs[i] = fx.add(600, false, WithPlanLimit())
			}(i, fx)
		}
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
