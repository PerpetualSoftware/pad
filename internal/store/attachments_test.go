package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWorkspaceStorageInfo_NoOwner covers the "fresh install" /
// legacy-workspace path: workspace exists but has no owner_id.
// Expected: limit unlimited, plan empty, no override (matches the
// upload-time behavior in WorkspaceStorageLimit, which returns -1
// rather than rejecting the upload outright).
func TestWorkspaceStorageInfo_NoOwner(t *testing.T) {
	s := testStore(t)

	wsID := newID()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`INSERT INTO workspaces (id, slug, name, settings, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`),
		wsID, "no-owner", "No Owner", "{}", ts, ts); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	info, err := s.WorkspaceStorageInfo(wsID)
	if err != nil {
		t.Fatalf("WorkspaceStorageInfo: %v", err)
	}
	if info.UsedBytes != 0 {
		t.Errorf("used_bytes = %d, want 0", info.UsedBytes)
	}
	if info.LimitBytes != -1 {
		t.Errorf("limit_bytes = %d, want -1", info.LimitBytes)
	}
	if info.Plan != "" {
		t.Errorf("plan = %q, want empty", info.Plan)
	}
	if info.OverrideActive {
		t.Errorf("override_active = true, want false")
	}
}

// TestWorkspaceStorageInfo_FreePlanResolution exercises the full
// owner → plan → override resolution chain:
//
//  1. Free plan with no override → limit_bytes = DefaultFreeLimits, override_active=false
//  2. Free plan + storage_bytes override → limit_bytes = override value, override_active=true
//  3. Pro plan → limit_bytes = -1 unconditionally (Phase 1 quirk:
//     pro/self-hosted bypass override resolution; the flag still
//     surfaces the configured override for admin visibility)
func TestWorkspaceStorageInfo_FreePlanResolution(t *testing.T) {
	s := testStore(t)

	owner, err := s.CreateUser(models.UserCreate{
		Email:    "owner@example.com",
		Name:     "Owner",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetUserPlan(owner.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan(free): %v", err)
	}

	wsID := newID()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`INSERT INTO workspaces (id, slug, name, settings, owner_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`),
		wsID, "owned", "Owned", "{}", owner.ID, ts, ts); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	// 1. Free plan, no override.
	info, err := s.WorkspaceStorageInfo(wsID)
	if err != nil {
		t.Fatalf("WorkspaceStorageInfo: %v", err)
	}
	if info.Plan != "free" {
		t.Errorf("plan = %q, want free", info.Plan)
	}
	if info.LimitBytes != int64(DefaultFreeLimits.StorageBytes) {
		t.Errorf("limit_bytes = %d, want %d", info.LimitBytes, DefaultFreeLimits.StorageBytes)
	}
	if info.OverrideActive {
		t.Errorf("override_active = true, want false")
	}

	// 2. Free plan + override.
	if err := s.SetUserPlanOverrides(owner.ID, `{"storage_bytes":1073741824}`); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	info, err = s.WorkspaceStorageInfo(wsID)
	if err != nil {
		t.Fatalf("WorkspaceStorageInfo (override): %v", err)
	}
	if info.LimitBytes != 1073741824 {
		t.Errorf("limit_bytes with override = %d, want 1073741824", info.LimitBytes)
	}
	if !info.OverrideActive {
		t.Errorf("override_active = false, want true after setting override")
	}

	// 3. Pro plan: the limit is unlimited regardless of override (Phase 1).
	// The override_active flag still surfaces the configured override
	// so the admin UI can show it; it just doesn't affect the limit.
	if err := s.SetUserPlan(owner.ID, "pro", ""); err != nil {
		t.Fatalf("SetUserPlan(pro): %v", err)
	}
	info, err = s.WorkspaceStorageInfo(wsID)
	if err != nil {
		t.Fatalf("WorkspaceStorageInfo (pro): %v", err)
	}
	if info.LimitBytes != -1 {
		t.Errorf("pro plan limit_bytes = %d, want -1 (unlimited)", info.LimitBytes)
	}
	if !info.OverrideActive {
		t.Errorf("override_active should still be true for pro plan with configured override")
	}
}

// TestWorkspaceStorageInfo_TracksLiveAttachments inserts a few
// attachment rows directly and asserts SUM(size_bytes) shows up in
// used_bytes — and that soft-deleted rows are excluded so the user
// sees the post-delete value (Settings → Storage UX expectation).
func TestWorkspaceStorageInfo_TracksLiveAttachments(t *testing.T) {
	s := testStore(t)

	wsID := newID()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(s.q(`INSERT INTO workspaces (id, slug, name, settings, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`),
		wsID, "ws", "WS", "{}", ts, ts); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	mkAttach := func(size int64, deleted bool) {
		t.Helper()
		a := &models.Attachment{
			WorkspaceID: wsID,
			UploadedBy:  "system",
			StorageKey:  "fs:" + newID(),
			ContentHash: newID(),
			MimeType:    "image/png",
			SizeBytes:   size,
			Filename:    "x.png",
		}
		if err := s.CreateAttachment(a); err != nil {
			t.Fatalf("CreateAttachment: %v", err)
		}
		if deleted {
			if _, err := s.db.Exec(s.q(`UPDATE attachments SET deleted_at = ? WHERE id = ?`), ts, a.ID); err != nil {
				t.Fatalf("soft-delete: %v", err)
			}
		}
	}

	mkAttach(1024, false)
	mkAttach(2048, false)
	mkAttach(99999, true) // deleted — must not count

	info, err := s.WorkspaceStorageInfo(wsID)
	if err != nil {
		t.Fatalf("WorkspaceStorageInfo: %v", err)
	}
	if info.UsedBytes != 3072 {
		t.Errorf("used_bytes = %d, want 3072 (1024 + 2048; soft-deleted excluded)", info.UsedBytes)
	}
}
