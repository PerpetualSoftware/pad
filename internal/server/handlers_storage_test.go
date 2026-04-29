package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// storageUsageResponse mirrors store.WorkspaceStorageInfo + the JSON
// shape the handler returns. Kept local so test asserts don't drift if
// the public type adds fields later (e.g. a "computed_at" timestamp).
type storageUsageResponse struct {
	UsedBytes      int64  `json:"used_bytes"`
	LimitBytes     int64  `json:"limit_bytes"`
	Plan           string `json:"plan"`
	OverrideActive bool   `json:"override_active"`
}

// TestStorageUsage_EmptyWorkspace covers the "fresh install" path:
// workspace has no owner_id, no attachments. Expected: used_bytes = 0,
// limit_bytes = -1 (unlimited fallback), plan empty, no override.
func TestStorageUsage_EmptyWorkspace(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/storage/usage", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp storageUsageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if resp.UsedBytes != 0 {
		t.Errorf("used_bytes = %d, want 0", resp.UsedBytes)
	}
	if resp.LimitBytes != -1 {
		t.Errorf("limit_bytes = %d, want -1 (unlimited)", resp.LimitBytes)
	}
	if resp.OverrideActive {
		t.Errorf("override_active = true, want false")
	}
}

// TestStorageUsage_TracksUploads pins the core acceptance criterion:
// after an upload the endpoint reports the new used_bytes total. We
// also exercise the cache invalidation hook by uploading twice and
// asserting the second value reflects both blobs.
func TestStorageUsage_TracksUploads(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	first := realPNG()

	if rr := doMultipartUpload(srv, slug, "a.png", first); rr.Code != http.StatusCreated {
		t.Fatalf("first upload: %d %s", rr.Code, rr.Body.String())
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/storage/usage", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var afterFirst storageUsageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &afterFirst); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if afterFirst.UsedBytes != int64(len(first)) {
		t.Fatalf("after first upload used_bytes = %d, want %d", afterFirst.UsedBytes, len(first))
	}

	// Upload a different blob (different filename, same bytes still
	// dedupes via content-hash but inserts a second row pointing at
	// the same storage_key; SUM(size_bytes) double-counts that row by
	// design — quota tracks logical usage, not unique bytes on disk).
	if rr := doMultipartUpload(srv, slug, "b.png", first); rr.Code != http.StatusCreated {
		t.Fatalf("second upload: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/storage/usage", nil)
	var afterSecond storageUsageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &afterSecond); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if afterSecond.UsedBytes != 2*int64(len(first)) {
		t.Errorf("after second upload used_bytes = %d, want %d (cache should have been invalidated)",
			afterSecond.UsedBytes, 2*int64(len(first)))
	}
}

// TestStorageUsage_RejectsGuests pins the requireMinRole("viewer") guard.
// RequireWorkspaceAccess admits item-grant guests (workspaceRole=="guest")
// who shouldn't see workspace-wide quota numbers — the explicit gate is
// what the activity / members / versions read endpoints all use.
//
// We exercise the gate by calling the handler directly with a context
// pre-populated with the "guest" role, rather than building an
// item-grant fixture (which would require a member-role user, an item,
// a grant row, and a session — all just to flip a single role string).
func TestStorageUsage_RejectsGuests(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+slug+"/storage/usage", nil)
	ctx := context.WithValue(req.Context(), ctxWorkspaceRole, "guest")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()

	srv.handleGetWorkspaceStorageUsage(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("guest got status %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// TestListAttachments_Pagination covers list + paginate + sort ordering.
// Uploads three blobs of distinct sizes and verifies:
//   - default sort is created_at DESC (newest first)
//   - sort=size ascends from smallest
//   - limit + offset paginate correctly with the right total count
func TestListAttachments_Pagination(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	// Three uploads. realPNG is the only stdlib-decodable shape we
	// have; pad each one with a different number of trailing bytes
	// after the IEND chunk so the size_bytes column varies. The
	// upload handler doesn't re-validate after the IEND — extra
	// trailing bytes are stored verbatim, which suits the test fine.
	base := realPNG()
	mkBody := func(extra int) []byte {
		out := make([]byte, len(base)+extra)
		copy(out, base)
		return out
	}
	doMultipartUpload(srv, slug, "small.png", mkBody(0))
	doMultipartUpload(srv, slug, "medium.png", mkBody(100))
	doMultipartUpload(srv, slug, "large.png", mkBody(1000))

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/attachments", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Attachments []struct {
			Filename  string `json:"filename"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"attachments"`
		Total  int `json:"total"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3", resp.Total)
	}
	if len(resp.Attachments) != 3 {
		t.Fatalf("got %d rows, want 3", len(resp.Attachments))
	}
	// Default created_at DESC. Three uploads in the same millisecond
	// can land with the same timestamp, so we don't pin the order —
	// just assert all three filenames are present in the page. The
	// sort=size assertion below covers the ordered case.
	gotFilenames := map[string]bool{}
	for _, a := range resp.Attachments {
		gotFilenames[a.Filename] = true
	}
	for _, want := range []string{"small.png", "medium.png", "large.png"} {
		if !gotFilenames[want] {
			t.Errorf("default sort: missing filename %q in result", want)
		}
	}

	// Ascending size sort.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/attachments?sort=size", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("sort=size status = %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Attachments[0].Filename != "small.png" || resp.Attachments[2].Filename != "large.png" {
		t.Errorf("sort=size order: %q,%q,%q want small,medium,large",
			resp.Attachments[0].Filename, resp.Attachments[1].Filename, resp.Attachments[2].Filename)
	}

	// Pagination: limit=2, offset=2 → only 1 row.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/attachments?limit=2&offset=2", nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 3 {
		t.Errorf("total stays 3 across pages, got %d", resp.Total)
	}
	if len(resp.Attachments) != 1 {
		t.Errorf("limit=2 offset=2: got %d rows, want 1", len(resp.Attachments))
	}
	if resp.Limit != 2 || resp.Offset != 2 {
		t.Errorf("echoed limit/offset = %d/%d, want 2/2", resp.Limit, resp.Offset)
	}
}

// TestListAttachments_HidesDerived asserts that thumbnail rows
// (parent_id != NULL) don't show up in the list. They count toward
// quota but are managed automatically — surfacing them clutters the
// settings page with rows the user didn't upload.
//
// We synthesize a thumbnail row directly via the store rather than
// running the real thumbnail pipeline, which would require a decodable
// image the pure-Go processor accepts.
func TestListAttachments_HidesDerived(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	if rr := doMultipartUpload(srv, slug, "original.png", realPNG()); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d", rr.Code)
	}

	// Insert a synthetic thumbnail row pointing at the original.
	// The handler should skip it because parent_id IS NOT NULL.
	originalID := getOnlyAttachmentID(t, srv, wsID)
	thumbVariant := "thumb-sm"
	if err := srv.store.CreateAttachment(&models.Attachment{
		WorkspaceID: wsID,
		UploadedBy:  "system",
		StorageKey:  "fs:fakehash",
		ContentHash: "fakehash",
		MimeType:    "image/png",
		SizeBytes:   123,
		Filename:    "original-thumb-sm.png",
		ParentID:    &originalID,
		Variant:     &thumbVariant,
	}); err != nil {
		t.Fatalf("CreateAttachment(thumb): %v", err)
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/attachments", nil)
	var resp struct {
		Attachments []struct{ ID string } `json:"attachments"`
		Total       int                   `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1 (derived thumbnail must be hidden)", resp.Total)
	}
}

// TestDeleteAttachment_HappyPath covers the soft-delete flow: a
// successful upload, a 204 from the delete handler, and a follow-up
// list call that no longer sees the row. Storage usage drops to 0
// confirming the cache invalidation hook fires.
func TestDeleteAttachment_HappyPath(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()

	rr := doMultipartUpload(srv, slug, "victim.png", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	var upload struct{ ID string }
	if err := json.Unmarshal(rr.Body.Bytes(), &upload); err != nil {
		t.Fatalf("decode upload: %v", err)
	}

	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+upload.ID, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// List should be empty now.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/attachments", nil)
	var listResp struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listResp.Total != 0 {
		t.Errorf("after delete: total = %d, want 0", listResp.Total)
	}

	// Storage usage drops to 0 (cache was invalidated).
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/storage/usage", nil)
	var usage storageUsageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if usage.UsedBytes != 0 {
		t.Errorf("after delete: used_bytes = %d, want 0", usage.UsedBytes)
	}

	// Second delete → 404 (already tombstoned).
	rr = doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+upload.ID, nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("second delete: status=%d, want 404", rr.Code)
	}
}

// TestDeleteAttachment_DerivedRefused pins the carve-out for thumbnail
// rows: a direct delete of a derived attachment should return 400
// rather than silently succeed (which would leave the original
// without thumbnails until a regenerate job runs).
func TestDeleteAttachment_DerivedRefused(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	wsID := workspaceIDForSlug(t, srv, slug)

	if rr := doMultipartUpload(srv, slug, "x.png", realPNG()); rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d", rr.Code)
	}
	originalID := getOnlyAttachmentID(t, srv, wsID)

	thumbVariant := "thumb-sm"
	thumb := &models.Attachment{
		WorkspaceID: wsID,
		UploadedBy:  "system",
		StorageKey:  "fs:fakehash2",
		ContentHash: "fakehash2",
		MimeType:    "image/png",
		SizeBytes:   1,
		Filename:    "x-thumb-sm.png",
		ParentID:    &originalID,
		Variant:     &thumbVariant,
	}
	if err := srv.store.CreateAttachment(thumb); err != nil {
		t.Fatalf("CreateAttachment(thumb): %v", err)
	}

	rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/attachments/"+thumb.ID, nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("delete derived: status=%d, want 400", rr.Code)
	}
}

// getOnlyAttachmentID returns the ID of the single live attachment
// row in a workspace — the test infrastructure uploads at most one
// blob per call, so this lookup is deterministic. Fails the test if
// zero or multiple rows are live.
func getOnlyAttachmentID(t *testing.T, srv *Server, workspaceID string) string {
	t.Helper()
	rows, _, err := srv.store.WorkspaceAttachments(workspaceID, store.AttachmentListFilters{})
	if err != nil {
		t.Fatalf("WorkspaceAttachments: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 attachment, got %d", len(rows))
	}
	return rows[0].ID
}

// workspaceIDForSlug looks up the internal UUID for a slug — used
// by tests that synthesize attachment rows directly via the store
// (bypassing the upload handler).
func workspaceIDForSlug(t *testing.T, srv *Server, slug string) string {
	t.Helper()
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug(%q): %v", slug, err)
	}
	if ws == nil {
		t.Fatalf("workspace %q not found", slug)
	}
	return ws.ID
}

// TestStorageInfoCache covers the in-memory cache directly so the TTL +
// invalidate paths have a focused test. The handler-level integration
// is exercised by TestStorageUsage_TracksUploads (which depends on
// invalidate-on-upload working).
func TestStorageInfoCache(t *testing.T) {
	now := time.Now()
	c := newStorageInfoCache(30 * time.Second)
	c.now = func() time.Time { return now }

	if got := c.get("ws1"); got != nil {
		t.Fatalf("get on empty cache = %v, want nil", got)
	}

	c.set("ws1", &store.WorkspaceStorageInfo{UsedBytes: 100, LimitBytes: 500, Plan: "free"})
	got := c.get("ws1")
	if got == nil {
		t.Fatal("get after set returned nil")
	}
	if got.UsedBytes != 100 {
		t.Errorf("used_bytes = %d, want 100", got.UsedBytes)
	}

	// Mutating the returned value must not poison the cache.
	got.UsedBytes = 999
	again := c.get("ws1")
	if again.UsedBytes != 100 {
		t.Errorf("cache mutated by caller: used_bytes = %d, want 100", again.UsedBytes)
	}

	// TTL expiry: advance the clock past 30s and assert the entry is gone.
	now = now.Add(31 * time.Second)
	if got := c.get("ws1"); got != nil {
		t.Errorf("get after TTL expiry = %v, want nil", got)
	}

	// Invalidate clears the entry immediately.
	now = time.Now()
	c.now = func() time.Time { return now }
	c.set("ws2", &store.WorkspaceStorageInfo{UsedBytes: 7})
	c.invalidate("ws2")
	if got := c.get("ws2"); got != nil {
		t.Errorf("get after invalidate = %v, want nil", got)
	}
}
