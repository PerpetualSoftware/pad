package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG-3153 at the doors. The character rule itself is pinned in
// internal/attachments (TestNormalizeFilename); these show the upload door
// and the download header apply it. The import door calls the same
// NormalizeFilename, and BUG-2818's bundle test shows it does.

// U+202E RIGHT-TO-LEFT OVERRIDE, percent-encoded as UTF-8 for the multipart
// filename parameter.
const rloEncoded = "%E2%80%AE"

// The spoof is stored without its override: "x<RLO>gvs.txt" renders as
// "xtxt.svg" in a bidi-aware UI; stored, it is the .txt it always was.
func TestUpload_BUG3153_BidiOverrideIsStrippedNotRefused(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	rr := uploadEncodedName(t, srv, slug, "x"+rloEncoded+"gvs.txt", []byte("plain text\n"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Filename != "xgvs.txt" {
		t.Errorf("stored filename = %q, want %q", got.Filename, "xgvs.txt")
	}
}

// An override INSIDE the extension hid it from the blocklist, which judged
// ".s<RLO>vg". The normalised name carries ".svg" and is refused.
func TestUpload_BUG3153_OverrideHidingABlockedExtensionIsRefused(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	rr := uploadEncodedName(t, srv, slug, "x.s"+rloEncoded+"vg", svgScript)
	if rr.Code != http.StatusUnsupportedMediaType || !strings.Contains(rr.Body.String(), "extension_blocked") {
		t.Errorf("status=%d body=%s; want 415 extension_blocked", rr.Code, rr.Body.String())
	}
}

// A LEGACY row is served without the characters, and one hiding a blocked
// extension is served as .bin, before the startup backfill has rewritten it.
func TestDownload_BUG3153_LegacyBidiRowIsServedClean(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	for _, c := range []struct{ legacy, want string }{
		{"x\u202Egvs.txt", "xgvs.txt"},
		{"x.s\u202Evg", "x.bin"},
	} {
		id, _, urlPath := uploadHelper(t, srv, slug, "legacy.xml", svgScript)
		if _, err := srv.store.DB().Exec(`UPDATE attachments SET filename = ? WHERE id = ?`, c.legacy, id); err != nil {
			t.Fatalf("plant legacy name %q: %v", c.legacy, err)
		}
		req := httptest.NewRequest("GET", urlPath, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%q: status=%d body=%s", c.legacy, rr.Code, rr.Body.String())
		}
		if served := dispositionFilename(t, rr.Header().Get("Content-Disposition")); served != c.want {
			t.Errorf("legacy %q served as %q, want %q", c.legacy, served, c.want)
		}
	}
}
