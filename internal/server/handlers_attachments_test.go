package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// testServerWithAttachments returns a fresh test server with the
// attachment registry wired against an FSStore rooted in t.TempDir(),
// and (on the pure-Go default build) the image processor wired so
// thumbnail derivation (TASK-878) runs end-to-end in tests that
// upload images. The image processor wiring is delegated to
// wireTestImageProcessor — split across two build-tagged files
// (`testimageprocessor_purego_test.go` / `_libvips_test.go`) so
// `go test -tags libvips ./internal/server` doesn't panic on the
// not-yet-implemented libvips NewProcessor.
func testServerWithAttachments(t *testing.T) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	dir := t.TempDir()
	fs, err := attachments.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	srv.SetAttachments(reg, 0)
	wireTestImageProcessor(srv)
	slug := createWSForTest(t, srv)
	return srv, slug
}

func doMultipartUpload(srv *Server, slug, filename string, body []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", filename)
	part.Write(body)
	mw.Close()

	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// realPNG returns a tiny but real PNG (1x1 transparent) so image.DecodeConfig succeeds.
func realPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
}

func TestUpload_HappyPathPNG(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()
	rr := doMultipartUpload(srv, slug, "screenshot.png", body)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID         string `json:"id"`
		URL        string `json:"url"`
		MIME       string `json:"mime"`
		Size       int64  `json:"size"`
		Width      *int   `json:"width"`
		Height     *int   `json:"height"`
		Filename   string `json:"filename"`
		Category   string `json:"category"`
		RenderMode string `json:"render_mode"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	if resp.ID == "" {
		t.Fatal("response missing id")
	}
	if resp.MIME != "image/png" {
		t.Errorf("mime = %q, want image/png", resp.MIME)
	}
	if resp.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", resp.Size, len(body))
	}
	if resp.Width == nil || *resp.Width != 1 || resp.Height == nil || *resp.Height != 1 {
		t.Errorf("width/height = %v / %v, want 1/1", resp.Width, resp.Height)
	}
	if resp.Category != "image" {
		t.Errorf("category = %q, want image", resp.Category)
	}
	if resp.RenderMode != "inline" {
		t.Errorf("render_mode = %q, want inline", resp.RenderMode)
	}
	// URL is the slug-based download path. Now that TASK-872 ships, the
	// upload response includes it so the editor doesn't have to build
	// the path itself.
	if !strings.Contains(resp.URL, "/attachments/"+resp.ID) || !strings.Contains(resp.URL, "/workspaces/"+slug) {
		t.Errorf("url = %q, want to contain workspace slug %q and attachment id %q", resp.URL, slug, resp.ID)
	}
}

// uploadAsGuest drives handleUploadAttachment directly with a synthesized
// request context (workspace + guest user + "guest" role), mirroring the
// direct-handler pattern TestStorageUsage_RejectsGuests uses. itemID, when
// non-empty, rides in the ?item_id query string so the handler authorizes
// via the item's grant chain (BUG-1661).
func uploadAsGuest(srv *Server, wsID, itemID string, guest *models.User, body []byte) *httptest.ResponseRecorder {
	return uploadAsGuestChannels(srv, wsID, itemID, "", guest, body)
}

// uploadAsGuestChannels is uploadAsGuest with independent control over the
// two item_id input channels: queryItemID rides the query string (the web
// client's shape) and formItemID rides the multipart form (the CLI's).
// Either, both, or neither may be empty.
func uploadAsGuestChannels(srv *Server, wsID, queryItemID, formItemID string, guest *models.User, body []byte) *httptest.ResponseRecorder {
	var queryIDs, formIDs []string
	if queryItemID != "" {
		queryIDs = []string{queryItemID}
	}
	if formItemID != "" {
		formIDs = []string{formItemID}
	}
	return uploadAsGuestRepeated(srv, wsID, queryIDs, formIDs, guest, body)
}

// uploadAsGuestRepeated is uploadAsGuestChannels with an arbitrary number of
// item_id values on each channel.
func uploadAsGuestRepeated(srv *Server, wsID string, queryIDs, formIDs []string, guest *models.User, body []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, id := range formIDs {
		mw.WriteField("item_id", id)
	}
	part, _ := mw.CreateFormFile("file", "shot.png")
	part.Write(body)
	mw.Close()

	req := httptest.NewRequest("POST", uploadPathWithItemIDs("/api/v1/workspaces/"+wsID+"/attachments", queryIDs), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "127.0.0.1:1234"

	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, wsID)
	ctx = context.WithValue(ctx, ctxCurrentUser, guest)
	ctx = context.WithValue(ctx, ctxWorkspaceRole, "guest")
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleUploadAttachment(rr, req)
	return rr
}

// uploadPathWithItemIDs appends any number of item_id query parameters.
func uploadPathWithItemIDs(path string, ids []string) string {
	for i, id := range ids {
		if i == 0 {
			path += "?"
		} else {
			path += "&"
		}
		path += "item_id=" + url.QueryEscape(id)
	}
	return path
}

// TestUpload_GrantBasedEditorCanAttach covers BUG-1661: a guest holding an
// item-level edit grant (but no workspace editor role) can upload an
// attachment when the request carries the granted item's ?item_id. The
// old flat requireMinRole("editor") gate rejected this with 403 even though
// the editor / comment composer offered the paste/drop affordance.
//
// The companion assertion (no item_id → 403) confirms the editor-role
// fallback for free-floating uploads didn't widen guest access.
func TestUpload_GrantBasedEditorCanAttach(t *testing.T) {
	srv, _ := testServerWithAttachments(t)

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Grants"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Granted", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Guest: not a workspace editor, holds an item edit grant only.
	guest, err := srv.store.CreateUser(models.UserCreate{
		Email:    "guest@test.com",
		Name:     "Guest",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, item.ID, guest.ID, "edit", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	// With the granted item's ?item_id → authorized via the grant chain.
	rr := uploadAsGuest(srv, ws.ID, item.ID, guest, realPNG())
	if rr.Code != http.StatusCreated {
		t.Fatalf("granted upload: status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}

	// Without item context → falls back to the workspace editor-role gate,
	// which this guest fails.
	rr = uploadAsGuest(srv, ws.ID, "", guest, realPNG())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("free-floating guest upload: status = %d, want 403; body = %s", rr.Code, rr.Body.String())
	}

	// The regression PLAN-2391 DR-2 exists for: the CLI sends item_id in
	// the multipart FORM only. The no-item editor gate used to fire before
	// the body was parsed, so this guest was 403'd before the association
	// that authorizes them was ever read.
	rr = uploadAsGuestChannels(srv, ws.ID, "", item.ID, guest, realPNG())
	if rr.Code != http.StatusCreated {
		t.Fatalf("form-only granted upload: status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	if got := attachmentItemID(t, srv, uploadedAttachmentID(t, rr)); got != item.ID {
		t.Errorf("form-only upload stored item_id = %q, want %q", got, item.ID)
	}

	// Both channels, agreeing (the web client's shape).
	rr = uploadAsGuestChannels(srv, ws.ID, item.ID, item.ID, guest, realPNG())
	if rr.Code != http.StatusCreated {
		t.Fatalf("both-channel granted upload: status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}

	// An item this guest holds NO grant on answers 404 — the same answer an
	// item_id that doesn't exist gets, so the status split can't be used as
	// an existence oracle for items the caller can't see.
	ungranted, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Not granted", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	for _, tc := range []struct{ name, query, form string }{
		{"query channel", ungranted.ID, ""},
		{"form channel", "", ungranted.ID},
	} {
		rr = uploadAsGuestChannels(srv, ws.ID, tc.query, tc.form, guest, realPNG())
		if rr.Code != http.StatusNotFound {
			t.Errorf("ungranted item upload (%s): status = %d, want 404; body = %s",
				tc.name, rr.Code, rr.Body.String())
		}
	}

	// Pairing a granted id with a probe id must not turn the conflict
	// rejection into an existence oracle: a hidden item and a nonexistent
	// one both answer 404, never 400.
	for _, tc := range []struct {
		name  string
		probe string
	}{
		{"hidden item", ungranted.ID},
		{"nonexistent item", "no-such-item"},
	} {
		rr = uploadAsGuestRepeated(srv, ws.ID, nil, []string{item.ID, tc.probe}, guest, realPNG())
		if rr.Code != http.StatusNotFound {
			t.Errorf("granted id + %s: status = %d, want 404; body = %s",
				tc.name, rr.Code, rr.Body.String())
		}
	}
}

// TestUpload_RejectsTooManyItemIDValues pins the bound on per-channel
// item_id values. Exact-string dedup can't bound the ResolveItem lookups on
// its own — TASK-7, task-7 and TASK-0007 are distinct strings that resolve
// to the same item — so the count is capped outright.
func TestUpload_RejectsTooManyItemIDValues(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	slug, item := uploadItemFixture(t, srv, "Flood")

	ids := make([]string, maxUploadItemIDValues+1)
	for i := range ids {
		ids[i] = item.ID
	}

	rr := uploadRepeatedItemIDs(srv, slug, ids, nil, realPNG())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("flooded query item_id: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	rr = uploadRepeatedItemIDs(srv, slug, nil, ids, realPNG())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("flooded form item_id: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}

	// The cap itself is still accepted.
	rr = uploadRepeatedItemIDs(srv, slug, nil, ids[:maxUploadItemIDValues], realPNG())
	if rr.Code != http.StatusCreated {
		t.Fatalf("at-cap form item_id: status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
}

// uploadedAttachmentID pulls the new attachment's id out of a 201 upload
// response body.
func uploadedAttachmentID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload response: %v body=%s", err, rr.Body.String())
	}
	if resp.ID == "" {
		t.Fatalf("upload response missing id: %s", rr.Body.String())
	}
	return resp.ID
}

// attachmentItemID reads back the persisted item_id for an attachment,
// returning "" for a NULL association.
func attachmentItemID(t *testing.T, srv *Server, attachmentID string) string {
	t.Helper()
	att, err := srv.store.GetAttachment(attachmentID)
	if err != nil {
		t.Fatalf("GetAttachment: %v", err)
	}
	if att == nil {
		t.Fatalf("attachment %s not found", attachmentID)
	}
	if att.ItemID == nil {
		return ""
	}
	return *att.ItemID
}

// uploadItemFixture creates a workspace + collection + item for the
// item_id-invariant tests, returning the workspace slug and the item.
func uploadItemFixture(t *testing.T, srv *Server, wsName string) (string, *models.Item) {
	t.Helper()
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: wsName})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Target", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return ws.Slug, item
}

// uploadChannels drives the real route (no users exist in these tests, so
// RequireWorkspaceAccess grants implicit owner) with independent control
// over the query-string and multipart-form item_id channels.
func uploadChannels(srv *Server, slug, queryItemID, formItemID string, body []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if formItemID != "" {
		mw.WriteField("item_id", formItemID)
	}
	part, _ := mw.CreateFormFile("file", "shot.png")
	part.Write(body)
	mw.Close()

	path := "/api/v1/workspaces/" + slug + "/attachments"
	if queryItemID != "" {
		path += "?item_id=" + queryItemID
	}
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// TestUpload_ItemIDStoredAsCanonicalUUID covers PLAN-2391 DR-2: ResolveItem
// accepts a UUID, a ref, or a slug, so the caller's spelling must never be
// what lands in attachments.item_id — the resolved UUID must.
func TestUpload_ItemIDStoredAsCanonicalUUID(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	slug, item := uploadItemFixture(t, srv, "Canonical")

	cases := []struct {
		name        string
		query, form string
	}{
		{"query slug", item.Slug, ""},
		{"form slug", "", item.Slug},
		{"different spellings, same item", item.Slug, item.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := uploadChannels(srv, slug, tc.query, tc.form, realPNG())
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
			}
			if got := attachmentItemID(t, srv, uploadedAttachmentID(t, rr)); got != item.ID {
				t.Errorf("stored item_id = %q, want canonical %q", got, item.ID)
			}
		})
	}
}

// TestUpload_RejectsUnresolvableItemID pins the status contract: an item_id
// that does not resolve in the request workspace is a 404 on either channel
// — including a well-formed UUID belonging to another workspace, which is
// exactly the malformed cross-workspace row BUG-2387 leaks through.
func TestUpload_RejectsUnresolvableItemID(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	slug, _ := uploadItemFixture(t, srv, "Home")
	_, foreign := uploadItemFixture(t, srv, "Foreign")

	cases := []struct {
		name        string
		query, form string
	}{
		{"query garbage", "no-such-item", ""},
		{"form garbage", "", "no-such-item"},
		{"query cross-workspace uuid", foreign.ID, ""},
		{"form cross-workspace uuid", "", foreign.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countAttachments(t, srv, slug)
			rr := uploadChannels(srv, slug, tc.query, tc.form, realPNG())
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rr.Code, rr.Body.String())
			}
			if after := countAttachments(t, srv, slug); after != before {
				t.Errorf("attachment rows = %d, want unchanged at %d", after, before)
			}
		})
	}
}

// TestUpload_HiddenAndMissingItemsAreIndistinguishable pins the existence
// oracle closed. resolveUploadItemID visibility-gates each resolved item
// BEFORE comparing channels, so a caller cannot use the 400-vs-404 split to
// probe for items they cannot see. That ordering is only half the fix: the
// two 404s must also be byte-identical, or the error code/message leaks the
// same bit ("item_not_found" => it really is absent; "not_found" => it
// exists but is hidden from you).
//
// Found by the Codex review of TASK-2400 after the ordering fix landed.
func TestUpload_HiddenAndMissingItemsAreIndistinguishable(t *testing.T) {
	srv, _ := testServerWithAttachments(t)

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Oracle"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Schema: `{"fields":[]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	hidden, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title: "Hidden", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// A guest with NO grant on the item: it exists, but is invisible to them.
	guest, err := srv.store.CreateUser(models.UserCreate{
		Email:    "prober@test.com",
		Name:     "Prober",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	existsButHidden := uploadAsGuestChannels(srv, ws.ID, hidden.ID, "", guest, realPNG())
	doesNotExist := uploadAsGuestChannels(srv, ws.ID, "00000000-0000-4000-8000-000000000000", "", guest, realPNG())

	if existsButHidden.Code != http.StatusNotFound || doesNotExist.Code != http.StatusNotFound {
		t.Fatalf("status: hidden = %d, missing = %d; want 404 for both",
			existsButHidden.Code, doesNotExist.Code)
	}
	if existsButHidden.Body.String() != doesNotExist.Body.String() {
		t.Errorf("404 bodies differ, leaking item existence:\n hidden  = %s\n missing = %s",
			existsButHidden.Body.String(), doesNotExist.Body.String())
	}
}

// TestUpload_RejectsConflictingItemIDChannels covers the other half of the
// status contract: two channels that each resolve, but to DIFFERENT items,
// is malformed input → 400. (Two spellings of the same item is not a
// conflict — see TestUpload_ItemIDStoredAsCanonicalUUID.)
func TestUpload_RejectsConflictingItemIDChannels(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	slug, item := uploadItemFixture(t, srv, "Conflict")
	other, err := srv.store.CreateItem(item.WorkspaceID, item.CollectionID, models.ItemCreate{
		Title: "Other", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	before := countAttachments(t, srv, slug)
	rr := uploadChannels(srv, slug, item.ID, other.ID, realPNG())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if after := countAttachments(t, srv, slug); after != before {
		t.Errorf("attachment rows = %d, want unchanged at %d", after, before)
	}
}

// TestUpload_RejectsRepeatedConflictingItemID covers the within-channel
// case: net/http keeps only the first value of a repeated field, so a
// first-wins read would authorize and associate item A while silently
// discarding a second, different item_id the caller also sent. Every value
// on a channel is resolved, and they must agree.
func TestUpload_RejectsRepeatedConflictingItemID(t *testing.T) {
	srv, _ := testServerWithAttachments(t)
	slug, item := uploadItemFixture(t, srv, "Repeated")
	other, err := srv.store.CreateItem(item.WorkspaceID, item.CollectionID, models.ItemCreate{
		Title: "Other", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Repeated in the query string.
	before := countAttachments(t, srv, slug)
	rr := uploadRepeatedItemIDs(srv, slug, []string{item.ID, other.ID}, nil, realPNG())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("repeated query item_id: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}

	// Repeated in the multipart form.
	rr = uploadRepeatedItemIDs(srv, slug, nil, []string{item.ID, other.ID}, realPNG())
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("repeated form item_id: status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if after := countAttachments(t, srv, slug); after != before {
		t.Errorf("attachment rows = %d, want unchanged at %d", after, before)
	}

	// Two spellings of the SAME item repeated on one channel is agreement,
	// not conflict.
	rr = uploadRepeatedItemIDs(srv, slug, nil, []string{item.ID, item.Slug}, realPNG())
	if rr.Code != http.StatusCreated {
		t.Fatalf("repeated same-item form item_id: status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	if got := attachmentItemID(t, srv, uploadedAttachmentID(t, rr)); got != item.ID {
		t.Errorf("stored item_id = %q, want %q", got, item.ID)
	}
}

// uploadRepeatedItemIDs drives the upload route with an arbitrary number of
// item_id values on each channel.
func uploadRepeatedItemIDs(srv *Server, slug string, queryIDs, formIDs []string, body []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, id := range formIDs {
		mw.WriteField("item_id", id)
	}
	part, _ := mw.CreateFormFile("file", "shot.png")
	part.Write(body)
	mw.Close()

	req := httptest.NewRequest("POST", uploadPathWithItemIDs("/api/v1/workspaces/"+slug+"/attachments", queryIDs), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// countAttachments counts live attachment rows in a workspace.
func countAttachments(t *testing.T, srv *Server, slug string) int {
	t.Helper()
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%q): %v", slug, err)
	}
	_, total, err := srv.store.WorkspaceAttachments(ws.ID, store.AttachmentListFilters{Limit: 200})
	if err != nil {
		t.Fatalf("WorkspaceAttachments: %v", err)
	}
	return total
}

// TestUpload_RemovesMultipartSpool covers the third leg of PLAN-2391 DR-2:
// file.Close() closes the spooled multipart temp file but never removes it
// — only r.MultipartForm.RemoveAll() does, and it has to run on EVERY exit
// path, success included.
//
// This needs a deliberate fixture: net/http only spills to disk past
// multipartParseMemory (1 MiB), so the tiny in-memory bodies the other
// upload tests use would pass whether or not the leak is fixed. TMPDIR is
// redirected at a dedicated dir so the assertion is "nothing at all is
// left behind" rather than a fragile diff against the shared system temp.
func TestUpload_RemovesMultipartSpool(t *testing.T) {
	// Build the fixture BEFORE redirecting TMPDIR — t.TempDir() resolves
	// against os.TempDir() at call time, so the store/blob dirs would
	// otherwise land inside spoolDir and defeat the emptiness assertion.
	srv, _ := testServerWithAttachments(t)
	slug, item := uploadItemFixture(t, srv, "Spool")

	spoolDir := t.TempDir()
	t.Setenv("TMPDIR", spoolDir)

	// > multipartParseMemory so net/http spills the part to disk. The
	// MIME sniff only reads the first 512 bytes, so a real PNG header
	// with a large zero tail is accepted.
	big := append(realPNG(), make([]byte, 2*multipartParseMemory)...)

	assertSpoolDirEmpty := func(t *testing.T, when string) {
		t.Helper()
		entries, err := os.ReadDir(spoolDir)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", spoolDir, err)
		}
		if len(entries) != 0 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("%s: temp dir not empty, leftover files: %v", when, names)
		}
	}

	// Success path — the leak PLAN-2391 DR-2 notes exists here too.
	rr := uploadChannels(srv, slug, item.ID, "", big)
	if rr.Code != http.StatusCreated {
		t.Fatalf("spooled upload: status = %d, want 201; body = %s", rr.Code, rr.Body.String())
	}
	assertSpoolDirEmpty(t, "after successful upload")

	// Post-parse rejection path — the form item_id can only be judged
	// after the body has already been spooled.
	rr = uploadChannels(srv, slug, "", "no-such-item", big)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("spooled rejection: status = %d, want 404; body = %s", rr.Code, rr.Body.String())
	}
	assertSpoolDirEmpty(t, "after post-parse rejection")
}

func TestUpload_RejectsExeAsPNG(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	exe := []byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff")
	rr := doMultipartUpload(srv, slug, "totally-safe.png", exe)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_RejectsExtensionMismatch(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	rr := doMultipartUpload(srv, slug, "evil.pdf", realPNG())
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_RejectsEmpty(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	rr := doMultipartUpload(srv, slug, "empty.png", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_RejectsMissingFilePart(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("notfile", "ignored")
	mw.Close()

	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_OverSizeLimit(t *testing.T) {
	srv := testServer(t)
	dir := t.TempDir()
	fs, _ := attachments.NewFSStore(dir)
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	// Set a tiny 1KiB cap so we don't have to allocate 25 MiB in the test.
	srv.SetAttachments(reg, 1024)
	slug := createWSForTest(t, srv)

	// Build a body larger than the cap. Use a 4KiB png header padded with PNG-like bytes.
	body := append(realPNG(), make([]byte, 4096)...)
	rr := doMultipartUpload(srv, slug, "big.png", body)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rr.Code, rr.Body.String())
	}
}

func TestUpload_DedupeSameContent(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()
	hash := sha256.Sum256(body)
	hashHex := hex.EncodeToString(hash[:])

	var ids []string
	for i := 0; i < 2; i++ {
		rr := doMultipartUpload(srv, slug, fmt.Sprintf("dup-%d.png", i), body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("upload %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
		var r struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &r)
		ids = append(ids, r.ID)
	}

	if ids[0] == ids[1] {
		t.Fatal("dedupe should produce two distinct attachment rows referencing the same blob")
	}

	// Look at the underlying store via the workspace usage helper —
	// SUM(size_bytes) for two rows of the same file should be 2x the bytes.
	// Resolve workspace ID via store.
	wsID := mustWorkspaceID(t, srv, slug)
	usage, err := srv.store.WorkspaceStorageUsage(wsID)
	if err != nil {
		t.Fatalf("WorkspaceStorageUsage: %v", err)
	}
	if usage != int64(2*len(body)) {
		t.Errorf("usage = %d, want %d (2 rows of same blob)", usage, 2*len(body))
	}

	// Both rows reference the same content_hash.
	att1, _ := srv.store.GetAttachment(ids[0])
	att2, _ := srv.store.GetAttachment(ids[1])
	if att1.ContentHash != att2.ContentHash || att1.ContentHash != hashHex {
		t.Errorf("hashes differ: %s vs %s vs expected %s", att1.ContentHash, att2.ContentHash, hashHex)
	}
	if att1.StorageKey != att2.StorageKey {
		t.Errorf("storage keys differ — dedupe should send both at fs:%s", hashHex)
	}
}

func TestUpload_ConcurrentSameFileNoCorruption(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	body := realPNG()

	const workers = 8
	var wg sync.WaitGroup
	codes := make([]int, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			rr := doMultipartUpload(srv, slug, fmt.Sprintf("c-%d.png", i), body)
			codes[i] = rr.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusCreated {
			t.Errorf("worker %d: status %d", i, c)
		}
	}

	// Verify the on-disk blob is still readable and matches our PNG bytes.
	wsID := mustWorkspaceID(t, srv, slug)
	usage, _ := srv.store.WorkspaceStorageUsage(wsID)
	if usage != int64(workers*len(body)) {
		t.Errorf("usage = %d, want %d (%d rows * %d bytes)", usage, workers*len(body), workers, len(body))
	}
}

// mustWorkspaceID resolves a workspace slug to its internal ID via the
// store. Tests use it because they don't have access to the request
// context that getWorkspaceID reads.
func mustWorkspaceID(t *testing.T, srv *Server, slug string) string {
	t.Helper()
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%q): %v", slug, err)
	}
	return ws.ID
}

// Ensure server.handleUploadAttachment 503s when no registry is wired
// (defensive check against future callers initializing the server
// without SetAttachments).
func TestUpload_NoRegistryWired(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	rr := doMultipartUpload(srv, slug, "x.png", realPNG())
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rr.Code, rr.Body.String())
	}
}

// TestUpload_QuotaCheckResolves regression-tests Codex round 1 finding 3:
// CheckLimit("storage_bytes") returns "unknown workspace feature", so the
// previous warning path silently dropped every probe. We now use
// WorkspaceStorageLimit which understands byte-counted features. The
// test asserts the storage helpers themselves return non-error values
// after a real upload — that is what the warning path needs to function.
func TestUpload_QuotaCheckResolves(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	rr := doMultipartUpload(srv, slug, "small.png", realPNG())
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", rr.Code)
	}

	wsID := mustWorkspaceID(t, srv, slug)
	usage, err := srv.store.WorkspaceStorageUsage(wsID)
	if err != nil {
		t.Fatalf("WorkspaceStorageUsage err: %v", err)
	}
	if usage <= 0 {
		t.Fatalf("usage = %d, want > 0", usage)
	}
	limit, err := srv.store.WorkspaceStorageLimit(wsID)
	if err != nil {
		t.Fatalf("WorkspaceStorageLimit err: %v (the warning path would silently drop this)", err)
	}
	// Self-hosted owner → -1 (unlimited). On other plans we'd see a positive limit.
	if limit < -1 {
		t.Errorf("limit = %d, want >= -1", limit)
	}
}

// Ensure unused imports don't cause CI issues even if helpers are removed.
var _ = io.Discard
var _ = models.Attachment{}
var _ = strings.Contains

// TestUpload_MultipartTextFieldsAreBindableText covers codex round 4's P2 on
// BUG-2803. The multipart body is deliberately exempt from the JSON NUL rule
// — its payload is binary blob content and must not be scanned for text
// validity — but its TEXT fields are a different thing. `item_id` goes to
// ResolveItem and into a database comparison exactly as the query-string
// channel does, and that channel has been validated at the transport since
// BUG-2784; the form channel was not. The FILENAME is the same shape: a
// multipart header can carry a NUL through the RFC 5987 encoded form.
//
// Both legs have a control that differs only in the bad byte, so a pass
// cannot come from the upload failing for an unrelated reason.
func TestUpload_MultipartTextFieldsAreBindableText(t *testing.T) {
	srv, slug := testServerWithAttachments(t)

	upload := func(t *testing.T, filename string, itemID *string) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if itemID != nil {
			// WriteField would reject nothing, which is the point: the raw
			// bytes reach r.MultipartForm.Value verbatim.
			_ = mw.WriteField("item_id", *itemID)
		}
		part, _ := mw.CreateFormFile("file", filename)
		part.Write(realPNG())
		mw.Close()

		req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/attachments", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.RemoteAddr = "127.0.0.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	// uploadWithRawDisposition writes the part header by hand so the test can
	// use the RFC 5987 encoded filename form. A RAW NUL in the header is not
	// the vector: Go's multipart reader refuses it as a malformed MIME header
	// line before any handler sees it (measured — the request answers 400
	// with "malformed MIME header line"). The percent-encoded form is
	// accepted by the header parser and decodes to the byte afterwards, which
	// is what makes it the reachable spelling.
	uploadWithRawDisposition := func(t *testing.T, disposition string) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", disposition)
		h.Set("Content-Type", "image/png")
		part, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		part.Write(realPNG())
		mw.Close()

		req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/attachments", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.RemoteAddr = "127.0.0.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	t.Run("filename carrying a NUL does not reach the attachment row", func(t *testing.T) {
		control := uploadWithRawDisposition(t,
			`form-data; name="file"; filename*=UTF-8''clean.png`)
		if control.Code != http.StatusCreated && control.Code != http.StatusOK {
			t.Fatalf("control upload must succeed, got %d: %s", control.Code, control.Body.String())
		}
		if !strings.Contains(control.Body.String(), "clean.png") {
			t.Fatalf("control should keep its encoded filename, got %s", control.Body.String())
		}

		rr := uploadWithRawDisposition(t,
			`form-data; name="file"; filename*=UTF-8''sh%00ot.png`)
		if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
			t.Fatalf("upload with an unusable filename should still succeed (the BYTES are fine), got %d: %s",
				rr.Code, rr.Body.String())
		}
		// Assert the REPLACEMENT, not the absence of a raw NUL byte. The
		// response is JSON, so a NUL in the filename comes back as the
		// six-character escape rather than as a 0x00 — a ContainsRune(body,
		// 0) check passes whether or not the fix is present, and it did:
		// disabling the fallback left this leg green until the assertion was
		// changed. CONVE-12, caught by the mutation rather than by review.
		var got struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode upload response: %v (body %s)", err, rr.Body.String())
		}
		if strings.ContainsRune(got.Filename, 0) {
			t.Errorf("the stored filename still carries a NUL: %q", got.Filename)
		}
		if got.Filename != "upload" {
			t.Errorf("expected the generic fallback name, got %q", got.Filename)
		}
	})

	t.Run("item_id carrying a NUL is not resolved", func(t *testing.T) {
		// A NUL-bearing item_id cannot name anything, so it must be treated
		// as no value rather than handed to the store. The control is a
		// syntactically fine but non-existent ref, which the handler answers
		// with a 4xx — if the NUL leg produced a 500 instead, the value
		// reached the database.
		// Assert it behaves like NO value, not merely "not a 500". A 500 is
		// the Postgres-only symptom; on SQLite an unfiltered value would
		// instead resolve to nothing and answer 4xx, so a >=500 check would
		// pass on this backend whether or not the filter exists.
		control := upload(t, "clean.png", nil)
		if control.Code != http.StatusCreated && control.Code != http.StatusOK {
			t.Fatalf("control (no item_id) must succeed, got %d: %s", control.Code, control.Body.String())
		}
		bad := "TASK-1\x00"
		rr := upload(t, "clean.png", &bad)
		if rr.Code != control.Code {
			t.Errorf("an unusable item_id must be treated as no value (like the control, %d), got %d: %s",
				control.Code, rr.Code, rr.Body.String())
		}
	})
}
