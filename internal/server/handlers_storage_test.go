package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

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
