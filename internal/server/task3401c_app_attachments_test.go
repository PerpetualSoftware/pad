package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3401 U6c: app attachments over the app API.

func appUpload(f appAPIFix, path string, body []byte, declared int64) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", f.path(path), bytes.NewReader(body))
	req.ContentLength = declared
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	return rr
}

func TestTask3401c_UploadStoresARowBoundToTheItem(t *testing.T) {
	f := appAPIFixture(t, "write")
	seqBefore := f.count(t, `SELECT seq FROM items WHERE id = ?`, f.item.ID)
	body := append(testPNG(t), 0) // different bytes: a fresh blob
	rr := appUpload(f, "/items/"+f.item.ID+"/attachments?filename=../../evil.png", body, int64(len(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	checkAppDTOKeys(t, "appUploadAttachment", got)
	if got["item_id"] != f.item.ID || got["mime_type"] != "image/png" || strings.Contains(got["filename"].(string), "/") {
		t.Errorf("upload DTO = %v", got)
	}
	var uploader, via string
	if err := f.srv.store.DB().QueryRow(`SELECT uploaded_by, via_app FROM attachments WHERE id = ?`, got["id"]).Scan(&uploader, &via); err != nil {
		t.Fatal(err)
	}
	if uploader != f.in.bot.ID || via != f.in.id {
		t.Errorf("row uploaded_by=%s via_app=%s, want the bot and the install", uploader, via)
	}
	if after := f.count(t, `SELECT seq FROM items WHERE id = ?`, f.item.ID); after != seqBefore {
		t.Errorf("an upload wrote the item row: seq %d -> %d", seqBefore, after)
	}
}

func TestTask3401c_UploadRefusals(t *testing.T) {
	f := appAPIFixture(t, "write")
	body := testPNG(t)
	human := createTestUserDirect(t, f.srv, "maker-3401c@example.com")
	humanItem, err := f.srv.store.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "Human made", ActorUserID: human.ID})
	if err != nil {
		t.Fatal(err)
	}
	before := f.count(t, `SELECT COUNT(*) FROM attachments`)
	cases := []struct {
		name     string
		path     string
		body     []byte
		declared int64
		status   int
	}{
		{"no Content-Length (chunked)", "/items/" + f.item.ID + "/attachments?filename=a.png", body, -1, http.StatusLengthRequired},
		{"over 25 MiB declared", "/items/" + f.item.ID + "/attachments?filename=a.png", body, 26 << 20, http.StatusRequestEntityTooLarge},
		{"body shorter than declared", "/items/" + f.item.ID + "/attachments?filename=a.png", body, int64(len(body)) + 10, http.StatusBadRequest},
		{"body longer than declared", "/items/" + f.item.ID + "/attachments?filename=a.png", body, int64(len(body)) - 10, http.StatusBadRequest},
		{"a type not on the allowlist", "/items/" + f.item.ID + "/attachments?filename=a.exe", []byte("MZ\x90\x00not really a program"), 23, http.StatusBadRequest},
		{"an item this app did not create", "/items/" + humanItem.ID + "/attachments?filename=a.png", body, int64(len(body)), http.StatusForbidden},
		{"an item outside the ceiling", "/items/" + f.privateItem.ID + "/attachments?filename=a.png", body, int64(len(body)), http.StatusNotFound},
	}
	for _, tc := range cases {
		rr := appUpload(f, tc.path, tc.body, tc.declared)
		if rr.Code != tc.status {
			t.Errorf("%s: %d %s, want %d", tc.name, rr.Code, rr.Body.String(), tc.status)
		}
	}
	if after := f.count(t, `SELECT COUNT(*) FROM attachments`); after != before {
		t.Errorf("refused uploads wrote %d rows", after-before)
	}
}

func TestTask3401c_ReadRule(t *testing.T) {
	f := appAPIFixture(t, "read")
	// A human's attachment on a human item in a companion is visible: the
	// read rule is companion binding, not who uploaded it.
	human := createTestUserDirect(t, f.srv, "reader-3401c@example.com")
	humanItem, err := f.srv.store.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "Human made", ActorUserID: human.ID})
	if err != nil {
		t.Fatal(err)
	}
	onHuman := seedAttachment(t, f, humanItem.ID, []byte("plain text body"), "text/plain", "notes.txt")
	if rr := appGet(f.srv, f.path("/attachments/"+onHuman), f.token); rr.Code != http.StatusOK {
		t.Errorf("a companion item's attachment: %d %s, want 200", rr.Code, rr.Body.String())
	}
	// Outside the ceiling and unknown answer byte-identically.
	private := seedAttachment(t, f, f.privateItem.ID, []byte("secret text"), "text/plain", "secret.txt")
	a := appGet(f.srv, f.path("/attachments/"+private), f.token)
	b := appGet(f.srv, f.path("/attachments/nope"), f.token)
	if a.Code != http.StatusNotFound || a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Errorf("private %d %q vs unknown %d %q: want identical 404s", a.Code, a.Body.String(), b.Code, b.Body.String())
	}
	for _, id := range []string{private, "nope"} {
		if rr := appGet(f.srv, f.path("/attachments/"+id+"/content"), f.token); rr.Code != http.StatusNotFound || bytes.Contains(rr.Body.Bytes(), []byte("secret")) {
			t.Errorf("download %s: %d %q, want 404 without the bytes", id, rr.Code, rr.Body.String())
		}
	}
}

func TestTask3401c_DownloadServesTheBytesFailClosed(t *testing.T) {
	f := appAPIFixture(t, "read")
	rr := appGet(f.srv, f.path("/attachments/"+f.attachment+"/content"), f.token)
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), f.attachmentBytes) {
		t.Fatalf("download: %d, %d bytes, want 200 and the %d stored bytes", rr.Code, rr.Body.Len(), len(f.attachmentBytes))
	}
	if rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(rr.Header().Get("Content-Disposition"), "inline") {
		t.Errorf("headers = %v", rr.Header())
	}
	// A variant that does not exist falls back to the original.
	if rr := appGet(f.srv, f.path("/attachments/"+f.attachment+"/content?variant=thumb-md"), f.token); rr.Code != http.StatusOK {
		t.Errorf("variant fallback: %d", rr.Code)
	}
	// Active content is never inline, and an unknown type is opaque.
	// Active content is never inline (DOC-3371 §4: HTML and SVG), and a type
	// not on the allowlist is served as opaque bytes.
	for _, mime := range []string{"image/svg+xml", "text/html", "application/x-weird"} {
		ct, disp := attachmentServeType(mime)
		if disp != "attachment" {
			t.Errorf("%s served %s", mime, disp)
		}
		if mime == "application/x-weird" && ct != "application/octet-stream" {
			t.Errorf("an unknown type served as %s", ct)
		}
	}
}

// Lead ruling (U6c): a download is re-admitted immediately before its first
// byte, and a revocation committed before then sends nothing.
func TestTask3401c_ARevocationBeforeTheFirstByteSendsNothing(t *testing.T) {
	for name, revoke := range map[string]string{
		"disable":          `UPDATE app_installs SET state = 'disabling', auth_epoch = auth_epoch + 1 WHERE id = ?`,
		"rotate":           `UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`,
		"companion leaves": `UPDATE collections SET via_app = NULL WHERE via_app = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := appAPIFixture(t, "read")
			ran := false
			f.srv.appBeforeFirstByte = func() {
				ran = true
				if _, err := f.srv.store.DB().Exec(revoke, f.in.id); err != nil {
					t.Error(err)
				}
			}
			rr := appGet(f.srv, f.path("/attachments/"+f.attachment+"/content"), f.token)
			if !ran {
				t.Fatal("control: the gate seam never ran")
			}
			if rr.Code != http.StatusUnauthorized || bytes.Contains(rr.Body.Bytes(), f.attachmentBytes[:16]) {
				t.Errorf("%s before the first byte: %d, %d bytes, want 401 and none of the file", name, rr.Code, rr.Body.Len())
			}
		})
	}
	// Control: without a revocation the same seam lets the bytes through.
	f := appAPIFixture(t, "read")
	f.srv.appBeforeFirstByte = func() {}
	if rr := appGet(f.srv, f.path("/attachments/"+f.attachment+"/content"), f.token); rr.Code != http.StatusOK {
		t.Errorf("control download: %d", rr.Code)
	}
}

// The route deadline ends a stream: the reader refuses once its context is
// done.
func TestTask3401c_TheStreamStopsAtTheDeadline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	rs := ctxReadSeeker{ctx: ctx, f: fh}
	buf := make([]byte, 4)
	if n, err := rs.Read(buf); n != 4 || err != nil {
		t.Fatalf("control read: %d %v", n, err)
	}
	cancel()
	if _, err := rs.Read(buf); !errors.Is(err, context.Canceled) {
		t.Errorf("read after the deadline: %v, want context.Canceled", err)
	}
	if _, err := io.ReadAll(rs); err == nil {
		t.Error("a stream past its deadline read on")
	}
}
