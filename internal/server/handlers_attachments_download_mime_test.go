package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2413: the attachment read path must fail CLOSED on Content-Disposition —
// a stored MIME that is not explicitly inline-safe is served as an attachment,
// so a legacy or mislabelled image/svg+xml, an extensionless SVG stored as
// text/xml, or an unrecognized row can never render as same-origin active
// content. The upload path rejects these MIME types today, so these tests seed
// rows DIRECTLY in the store to stand in for legacy / mislabelled rows.

// mimeDispoFixture is a server with an attachment registry plus a workspace,
// collection, and item to hang seeded rows on. Returns the workspace slug (for
// the request URL), the workspace id, and the item id.
func mimeDispoFixture(t *testing.T) (srv *Server, slug, wsID, itemID string) {
	t.Helper()
	srv = testServer(t)
	dir := t.TempDir()
	fs, err := attachments.NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	reg := attachments.NewRegistry()
	reg.Register(attachments.FSPrefix, fs)
	srv.SetAttachments(reg, 0)

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "MimeDispo"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	col, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Has attachments", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return srv, ws.Slug, ws.ID, item.ID
}

// seedStoredRow writes body to the registry and creates a live attachment row
// with an arbitrary stored MIME + filename attached to itemID. The row shape is
// the one a legacy / mislabelled upload would leave behind. Optional parentID +
// variant seed a derived (variant) row instead. Returns the new attachment id.
func seedStoredRow(t *testing.T, srv *Server, wsID, itemID, mime, filename string, body []byte, parentID, variant string) string {
	t.Helper()
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	st, err := srv.attachments.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		t.Fatalf("resolve store: %v", err)
	}
	key, err := st.Put(context.Background(), hash, mime, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	att := &models.Attachment{
		WorkspaceID: wsID,
		UploadedBy:  "system",
		StorageKey:  key,
		ContentHash: hash,
		MimeType:    mime,
		SizeBytes:   int64(len(body)),
		Filename:    filename,
	}
	if itemID != "" {
		iid := itemID
		att.ItemID = &iid
	}
	if parentID != "" {
		pid := parentID
		att.ParentID = &pid
	}
	if variant != "" {
		v := variant
		att.Variant = &v
	}
	if err := srv.store.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	return att.ID
}

// serve issues a GET (method="GET") or HEAD against a seeded row and returns the
// recorder. variant, when non-empty, is appended as ?variant=.
func serveAttachment(t *testing.T, srv *Server, slug, id, method, variant string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/v1/workspaces/" + slug + "/attachments/" + id
	if variant != "" {
		url += "?variant=" + variant
	}
	req := httptest.NewRequest(method, url, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func assertAttachmentDisposition(t *testing.T, rr *httptest.ResponseRecorder, wantPrefix string) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("Content-Disposition = %q, want %s...", got, wantPrefix)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

const svgBytes = `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`

// A legacy row stored as image/svg+xml is force-downloaded — GET and HEAD both.
func TestDownload_SVGLabeledRowForcedAsAttachment(t *testing.T) {
	srv, slug, wsID, itemID := mimeDispoFixture(t)
	id := seedStoredRow(t, srv, wsID, itemID, "image/svg+xml", "diagram.svg", []byte(svgBytes), "", "")

	for _, method := range []string{"GET", "HEAD"} {
		rr := serveAttachment(t, srv, slug, id, method, "")
		assertAttachmentDisposition(t, rr, "attachment;")
		if t.Failed() {
			t.Fatalf("%s: image/svg+xml served inline — active content reachable same-origin", method)
		}
	}
}

// An extensionless SVG that was sniffed and stored as text/xml (on the upload
// allowlist as a RenderChip type) is force-downloaded.
func TestDownload_TextXMLForcedAsAttachment(t *testing.T) {
	srv, slug, wsID, itemID := mimeDispoFixture(t)
	id := seedStoredRow(t, srv, wsID, itemID, "text/xml", "drawing", []byte(svgBytes), "", "")

	for _, method := range []string{"GET", "HEAD"} {
		rr := serveAttachment(t, srv, slug, id, method, "")
		assertAttachmentDisposition(t, rr, "attachment;")
	}
}

// An unrecognized stored MIME is force-downloaded AND served as opaque
// octet-stream bytes, never echoed back as a type the browser might act on.
func TestDownload_UnknownMIMEForcedAsAttachmentOctetStream(t *testing.T) {
	srv, slug, wsID, itemID := mimeDispoFixture(t)
	id := seedStoredRow(t, srv, wsID, itemID, "application/x-not-a-real-type", "mystery.bin", []byte("whatever"), "", "")

	for _, method := range []string{"GET", "HEAD"} {
		rr := serveAttachment(t, srv, slug, id, method, "")
		assertAttachmentDisposition(t, rr, "attachment;")
		if got := rr.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("%s Content-Type = %q, want application/octet-stream (unknown MIME must not be echoed back)", method, got)
		}
	}
}

// The variant endpoint shares the read handler, so the same disposition gate
// applies: a derived row wearing a non-inline-safe MIME is force-downloaded too
// (a contrived svg-typed variant stands in for the general case).
func TestDownload_VariantForcedAsAttachment(t *testing.T) {
	srv, slug, wsID, itemID := mimeDispoFixture(t)
	parent := seedStoredRow(t, srv, wsID, itemID, "image/png", "pic.png", realPNG(), "", "")
	// A derived variant carrying an unsafe MIME — the handler resolves it via the
	// ?variant= lookup and must run the disposition gate on the VARIANT's type.
	seedStoredRow(t, srv, wsID, itemID, "image/svg+xml", "pic.svg", []byte(svgBytes), parent, models.AttachmentVariantThumbMd)

	rr := serveAttachment(t, srv, slug, parent, "GET", models.AttachmentVariantThumbMd)
	assertAttachmentDisposition(t, rr, "attachment;")
}

// PDF stays inline (browser preview) — the fix must not break legitimate inline
// types.
func TestDownload_PDFStaysInline(t *testing.T) {
	srv, slug, wsID, itemID := mimeDispoFixture(t)
	id := seedStoredRow(t, srv, wsID, itemID, "application/pdf", "doc.pdf", []byte("%PDF-1.4 minimal"), "", "")

	rr := serveAttachment(t, srv, slug, id, "GET", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline; ... (PDF must stay previewable)", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", got)
	}
}

// Plain text stays inline.
func TestDownload_PlainTextStaysInline(t *testing.T) {
	srv, slug, wsID, itemID := mimeDispoFixture(t)
	id := seedStoredRow(t, srv, wsID, itemID, "text/plain", "notes.txt", []byte("hello"), "", "")

	rr := serveAttachment(t, srv, slug, id, "GET", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Errorf("Content-Disposition = %q, want inline; ... (text/plain must stay inline)", got)
	}
}
