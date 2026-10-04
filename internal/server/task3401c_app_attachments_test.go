package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	// The fail-closed type decision is the download's, not only a unit's: an
	// allowlisted active type is downloaded as an attachment, never inline.
	html := seedAttachment(t, f, f.item.ID, []byte("<html><script>alert(1)</script></html>"), "text/html", "page.html")
	if rr := appGet(f.srv, f.path("/attachments/"+html+"/content"), f.token); rr.Code != http.StatusOK ||
		!strings.HasPrefix(rr.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("an HTML download: %d, Content-Disposition %q, want attachment", rr.Code, rr.Header().Get("Content-Disposition"))
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
		// The subject's membership narrows: only the item re-check sees it,
		// since the fenced read rule asks about the companion alone.
		"membership narrows": `UPDATE workspace_members SET collection_access = 'specific' WHERE user_id = (SELECT bot_user_id FROM app_installs WHERE id = ?)`,
		// The attachment itself is deleted: only the attachment re-check sees
		// it, since its item is unchanged.
		"attachment deleted": `UPDATE attachments SET deleted_at = '2026-01-01T00:00:00Z' WHERE id = ?FROM-ATTACHMENT`,
	} {
		t.Run(name, func(t *testing.T) {
			f := appAPIFixture(t, "read")
			ran := false
			f.srv.appBeforeFirstByte = func() {
				ran = true
				arg := f.in.id
				if strings.Contains(revoke, "FROM-ATTACHMENT") {
					revoke, arg = strings.Replace(revoke, "FROM-ATTACHMENT", "", 1), f.attachment
				}
				if _, err := f.srv.store.DB().Exec(revoke, arg); err != nil {
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

// The subject's own visibility is a layer of its own: the fenced read rule
// asks only whether the attachment's item is a companion item, so a bot
// whose membership no longer reaches the companion (collection_access
// 'specific' with no grant) must still be refused here.
func TestTask3401c_TheSubjectMustSeeTheItem(t *testing.T) {
	f := appAPIFixture(t, "read")
	if rr := appGet(f.srv, f.path("/attachments/"+f.attachment), f.token); rr.Code != http.StatusOK {
		t.Fatalf("control: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := f.srv.store.DB().Exec(`UPDATE workspace_members SET collection_access = 'specific' WHERE workspace_id = ? AND user_id = ?`, f.ws.ID, f.in.bot.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/attachments/" + f.attachment, "/attachments/" + f.attachment + "/content"} {
		rr := appGet(f.srv, f.path(p), f.token)
		if rr.Code != http.StatusNotFound || bytes.Contains(rr.Body.Bytes(), f.attachmentBytes[:16]) {
			t.Errorf("%s with the membership narrowed: %d, want the attachment 404", p, rr.Code)
		}
	}
}

// The upload's write gate includes the subject's edit right: a bot whose
// membership role is viewer cannot upload, even with a write token. The
// fenced store checks the companion and the creator, not the role.
func TestTask3401c_AViewerCannotUpload(t *testing.T) {
	f := appAPIFixture(t, "write")
	body := testPNG(t)
	if _, err := f.srv.store.DB().Exec(`UPDATE workspace_members SET role = 'viewer' WHERE workspace_id = ? AND user_id = ?`, f.ws.ID, f.in.bot.ID); err != nil {
		t.Fatal(err)
	}
	before := f.count(t, `SELECT COUNT(*) FROM attachments`)
	rr := appUpload(f, "/items/"+f.item.ID+"/attachments?filename=a.png", body, int64(len(body)))
	if rr.Code != http.StatusForbidden {
		t.Errorf("a viewer's upload: %d %s, want 403", rr.Code, rr.Body.String())
	}
	if after := f.count(t, `SELECT COUNT(*) FROM attachments`); after != before {
		t.Errorf("a viewer's upload wrote %d rows", after-before)
	}
}

// Codex U6c r1 P1: a companion that stops being one while an upload's body
// is in flight. The fence checks the collection's stamp in the row's own
// transaction, so no row commits.
func TestTask3401c_ACompanionLeavingMidUploadCommitsNoRow(t *testing.T) {
	for name, change := range map[string]string{
		"deleted":    `UPDATE collections SET deleted_at = '2026-01-01T00:00:00Z' WHERE id = ?`,
		"re-stamped": `UPDATE collections SET via_app = NULL WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := appAPIFixture(t, "write")
			body := append(testPNG(t), 1, 2, 3)
			before := f.count(t, `SELECT COUNT(*) FROM attachments`)
			changed := false
			r := &lazyBody{build: func() []byte {
				if _, err := f.srv.store.DB().Exec(change, f.companion.ID); err != nil {
					t.Error(err)
				}
				changed = true
				return body
			}}
			req := httptest.NewRequest("POST", f.path("/items/"+f.item.ID+"/attachments?filename=a.png"), r)
			req.ContentLength = int64(len(body))
			req.Header.Set("Authorization", "Bearer "+f.token)
			req.RemoteAddr = "192.0.2.1:1234"
			rr := httptest.NewRecorder()
			f.srv.ServeHTTP(rr, req)
			if !changed {
				t.Fatal("control: the body was never read, so the change did not run mid-upload")
			}
			if rr.Code < 400 {
				t.Errorf("upload after the companion %s: %d %s, want a refusal", name, rr.Code, rr.Body.String())
			}
			if after := f.count(t, `SELECT COUNT(*) FROM attachments`); after != before {
				t.Errorf("a row committed after the companion was %s", name)
			}
		})
	}
}

// Codex U6c r1 P1: a credential change committed while the re-checks run.
// The credential is read again as re-admission's last step.
func TestTask3401c_ARevocationDuringReadmissionSendsNothing(t *testing.T) {
	for name, revoke := range map[string]string{
		"rotate":  `UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`,
		"disable": `UPDATE app_installs SET state = 'disabling', auth_epoch = auth_epoch + 1 WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := appAPIFixture(t, "read")
			ran := false
			f.srv.appAfterRechecks = func() {
				if ran {
					return
				}
				ran = true
				if _, err := f.srv.store.DB().Exec(revoke, f.in.id); err != nil {
					t.Error(err)
				}
			}
			rr := appGet(f.srv, f.path("/attachments/"+f.attachment+"/content"), f.token)
			if !ran {
				t.Fatal("control: the seam never ran")
			}
			if rr.Code != http.StatusUnauthorized || bytes.Contains(rr.Body.Bytes(), f.attachmentBytes[:16]) {
				t.Errorf("%s during re-admission: %d, want 401 and none of the file", name, rr.Code)
			}
		})
	}
}

// Codex U6c r1 P2: a storage error on an attachment the subject may not see
// answers exactly as an unknown id does.
func TestTask3401c_AMissingBlobIsNoOracle(t *testing.T) {
	f := appAPIFixture(t, "read")
	var key string
	if err := f.srv.store.DB().QueryRow(`SELECT storage_key FROM attachments WHERE id = ?`, f.attachment).Scan(&key); err != nil {
		t.Fatal(err)
	}
	blob, err := f.blobs.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	path := blob.Name()
	_ = blob.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.DB().Exec(`UPDATE workspace_members SET collection_access = 'specific' WHERE workspace_id = ? AND user_id = ?`, f.ws.ID, f.in.bot.ID); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"", "?variant=thumb-md"} {
		hidden := appGet(f.srv, f.path("/attachments/"+f.attachment+"/content"+v), f.token)
		unknown := appGet(f.srv, f.path("/attachments/nope/content"+v), f.token)
		if hidden.Code != unknown.Code || hidden.Body.String() != unknown.Body.String() {
			t.Errorf("hidden with a missing blob %d %q vs unknown %d %q", hidden.Code, hidden.Body.String(), unknown.Code, unknown.Body.String())
		}
	}
}

// Codex U6c r1 P2: the route deadline bounds the transport too. A client
// that reads the headers and then stops reading must not hold the handler
// in a blocked write past the deadline.
func TestTask3401c_AStalledClientDoesNotOutliveTheDeadline(t *testing.T) {
	f := appAPIFixture(t, "read")
	big := bytes.Repeat([]byte("pad stream test line\n"), (24<<20)/21)
	id := seedAttachment(t, f, f.item.ID, big, "text/plain", "big.txt")
	old := appAttachmentRouteDeadline
	appAttachmentRouteDeadline = 300 * time.Millisecond
	defer func() { appAttachmentRouteDeadline = old }()
	done := make(chan struct{}, 1)
	f.srv.appStreamDone = func() { done <- struct{}{} }
	ts := httptest.NewServer(f.srv)
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+f.path("/attachments/"+id+"/content"), nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control: %d", resp.StatusCode)
	}
	// Read nothing more: the server's writes block once the socket buffers
	// fill, far short of 24 MiB.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the download handler was still blocked writing 5s after its 300ms deadline")
	}
}

// Codex U6c r2: a body that ends early over a real connection reports
// io.ErrUnexpectedEOF, which is the client's mistake (400), not a 500.
func TestTask3401c_ATruncatedUploadIsTheClientsMistake(t *testing.T) {
	f := appAPIFixture(t, "write")
	ts := httptest.NewServer(f.srv)
	defer ts.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := append(testPNG(t), bytes.Repeat([]byte{0}, 1000)...)
	head := "POST " + f.path("/items/"+f.item.ID+"/attachments?filename=a.png") + " HTTP/1.1\r\n" +
		"Host: x\r\nAuthorization: Bearer " + f.token + "\r\nContent-Length: " + strconv.Itoa(len(body)+5000) + "\r\n\r\n"
	if _, err := conn.Write(append([]byte(head), body...)); err != nil {
		t.Fatal(err)
	}
	_ = conn.(*net.TCPConn).CloseWrite()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(got, []byte("size_mismatch")) {
		t.Errorf("a truncated upload: %d %s, want 400 size_mismatch", resp.StatusCode, got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM attachments`); n != 1 {
		t.Errorf("a truncated upload left %d rows, want only the fixture's", n)
	}
}

// Codex U6c r2: a store fault during re-admission withholds the bytes but
// answers 500, not the 401 invalid_token that tells the app to discard a
// good credential.
func TestTask3401c_AFaultDuringReadmissionIsNotADenial(t *testing.T) {
	// Each table is read by a different step of re-admission: the attachment
	// re-check, and the token's state (codex U6c r4).
	for _, table := range []string{"attachments", "app_token_bindings"} {
		t.Run(table, func(t *testing.T) {
			f := appAPIFixture(t, "read")
			f.srv.appBeforeFirstByte = func() {
				if _, err := f.srv.store.DB().Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_gone`); err != nil {
					t.Error(err)
				}
			}
			defer func() {
				_, _ = f.srv.store.DB().Exec(`ALTER TABLE ` + table + `_gone RENAME TO ` + table)
			}()
			rr := appGet(f.srv, f.path("/attachments/"+f.attachment+"/content"), f.token)
			if rr.Code != http.StatusInternalServerError || bytes.Contains(rr.Body.Bytes(), f.attachmentBytes[:16]) {
				t.Errorf("a fault during re-admission: %d %q, want 500 and none of the file", rr.Code, rr.Body.String())
			}
			if rr.Header().Get("WWW-Authenticate") != "" {
				t.Error("a fault answered with an invalid_token challenge")
			}
		})
	}
}

// Codex U6c r2: HEAD probes the download through the same gate, and an
// unknown variant name is refused as on the regular download.
func TestTask3401c_HeadAndUnknownVariant(t *testing.T) {
	f := appAPIFixture(t, "read")
	req := httptest.NewRequest("HEAD", f.path("/attachments/"+f.attachment+"/content"), nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.Len() != 0 || rr.Header().Get("Content-Length") != strconv.Itoa(len(f.attachmentBytes)) {
		t.Errorf("HEAD: %d, body %d bytes, Content-Length %q", rr.Code, rr.Body.Len(), rr.Header().Get("Content-Length"))
	}
	if rr := appGet(f.srv, f.path("/attachments/"+f.attachment+"/content?variant=nope"), f.token); rr.Code != http.StatusBadRequest {
		t.Errorf("an unknown variant: %d, want 400", rr.Code)
	}
}

// Codex U6c r3 P1: the subject's edit right, checked again once the body is
// in. A membership narrowed while the body was read refuses the row.
func TestTask3401c_ASubjectNarrowedMidUploadCommitsNoRow(t *testing.T) {
	for name, change := range map[string]string{
		"collection access narrowed": `UPDATE workspace_members SET collection_access = 'specific' WHERE user_id = ?`,
		"role dropped to viewer":     `UPDATE workspace_members SET role = 'viewer' WHERE user_id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := appAPIFixture(t, "write")
			body := append(testPNG(t), 4, 5, 6)
			before := f.count(t, `SELECT COUNT(*) FROM attachments`)
			changed := false
			r := &lazyBody{build: func() []byte {
				if _, err := f.srv.store.DB().Exec(change, f.in.bot.ID); err != nil {
					t.Error(err)
				}
				changed = true
				return body
			}}
			req := httptest.NewRequest("POST", f.path("/items/"+f.item.ID+"/attachments?filename=a.png"), r)
			req.ContentLength = int64(len(body))
			req.Header.Set("Authorization", "Bearer "+f.token)
			req.RemoteAddr = "192.0.2.1:1234"
			rr := httptest.NewRecorder()
			f.srv.ServeHTTP(rr, req)
			if !changed {
				t.Fatal("control: the body was never read")
			}
			if rr.Code != http.StatusForbidden {
				t.Errorf("%s mid-upload: %d %s, want 403", name, rr.Code, rr.Body.String())
			}
			if after := f.count(t, `SELECT COUNT(*) FROM attachments`); after != before {
				t.Errorf("a row committed after the %s", name)
			}
		})
	}
}

// Codex U6c r5: a credential that dies while an upload's body arrives is a
// 401 with the invalid_token challenge, not the 403 a permission change gets.
func TestTask3401c_ACredentialDyingMidUploadIs401(t *testing.T) {
	f := appAPIFixture(t, "write")
	body := append(testPNG(t), 7, 8, 9)
	r := &lazyBody{build: func() []byte {
		if _, err := f.srv.store.DB().Exec(`UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`, f.in.id); err != nil {
			t.Error(err)
		}
		return body
	}}
	req := httptest.NewRequest("POST", f.path("/items/"+f.item.ID+"/attachments?filename=a.png"), r)
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Errorf("a rotation mid-upload: %d %q, want 401 invalid_token", rr.Code, rr.Header().Get("WWW-Authenticate"))
	}
}
