package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BUG-2822 at the doors. The device-name rule itself is pinned in
// internal/attachments (TestNormalizeFilename); these show the upload door
// stores it and the download header serves it, for a new row and a legacy
// one. The import door calls the same NormalizeFilename, as BUG-3153's test
// file notes.

func TestUpload_BUG2822_WindowsDeviceNameIsPrefixed(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	for _, c := range []struct{ sent, want string }{
		{"nul.txt", "_nul.txt"},
		{"CON", "_CON"},
		// Preservation: a device name inside a longer stem is an ordinary name.
		{"console.txt", "console.txt"},
	} {
		rr := uploadEncodedName(t, srv, slug, c.sent, []byte("plain text\n"))
		if rr.Code != http.StatusCreated {
			t.Fatalf("%q: status=%d body=%s", c.sent, rr.Code, rr.Body.String())
		}
		var got struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("%q: decode: %v", c.sent, err)
		}
		if got.Filename != c.want {
			t.Errorf("%q stored as %q, want %q", c.sent, got.Filename, c.want)
		}
	}
}

// A row stored before BUG-2822 keeps its device name in the table; the
// download header serves it prefixed.
func TestDownload_BUG2822_LegacyDeviceNameIsServedPrefixed(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	id, _, urlPath := uploadHelper(t, srv, slug, "legacy.xml", svgScript)
	if _, err := srv.store.DB().Exec(`UPDATE attachments SET filename = ? WHERE id = ?`, "con.xml", id); err != nil {
		t.Fatalf("plant legacy name: %v", err)
	}
	req := httptest.NewRequest("GET", urlPath, nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if served := dispositionFilename(t, rr.Header().Get("Content-Disposition")); served != "_con.xml" {
		t.Errorf("legacy %q served as %q, want %q", "con.xml", served, "_con.xml")
	}
}
