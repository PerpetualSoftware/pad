package server

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http/httptest"
	"testing"
)

// The population: every class a stored name can be in. ASCII that travels
// plain; ASCII the quoted-string form cannot carry bare; runes strconv.IsPrint
// rejects (the `%q` corruption); multi-byte scripts; combining marks; emoji;
// and the characters RFC 8187 encoding and parameter parsing treat specially.
var contentDispositionNames = []string{
	"plain.txt", "with space.txt", "semi;colon.txt", "per%cent.txt", "star*.txt", "tick'.txt",
	`quo"te.txt`, `back\slash.txt`,
	"a b.txt", "a​b.txt", "a b.txt",
	"résumé.pdf", "会議.pdf", "é.txt", "📎 clip.png",
	// Characters the extended form must itself percent-encode, beside a rune
	// that forces that form: a bare '%' there would decode as an escape.
	"50% résumé.txt", "a'b é.txt", "é;x=y.txt",
}

func TestContentDispositionRoundTripsEveryName(t *testing.T) {
	for _, name := range contentDispositionNames {
		h := contentDisposition("attachment", name)
		disp, params, err := mime.ParseMediaType(h)
		if err != nil || disp != "attachment" || params["filename"] != name {
			t.Errorf("%q: header %q parsed to (%q, %q, %v)", name, h, disp, params["filename"], err)
		}
		for i := 0; i < len(h); i++ {
			if h[i] < 0x20 || h[i] > 0x7e {
				t.Errorf("%q: header %q carries a non-ASCII byte", name, h)
				break
			}
		}
	}
}

// A name that travels plain is byte-identical to the `%q` form every site
// used, so this change is invisible for every ASCII name in the product.
func TestContentDispositionPlainNameUnchanged(t *testing.T) {
	for _, name := range []string{"plain.txt", "with space.txt", "my-item.pad.md", "ws-export.tar.gz"} {
		if got, was := contentDisposition("inline", name), fmt.Sprintf(`inline; filename=%q`, name); got != was {
			t.Errorf("%q: %q, was %q", name, got, was)
		}
	}
	if got := contentDisposition("attachment", "会議.pdf"); got != `attachment; filename="__.pdf"; filename*=UTF-8''%E4%BC%9A%E8%AD%B0.pdf` {
		t.Errorf("CJK: %q", got)
	}
}

// Through the real upload and download doors: the name a Go client parses out
// of the download header is the name the server stored. NBSP and ZWSP were
// the measured corruption (BUG-3190 checkpoint 1).
func TestAttachmentDownloadCarriesTheStoredName(t *testing.T) {
	srv, slug := testServerWithAttachments(t)
	for _, name := range []string{"plain.txt", "résumé.txt", "会議.txt", "a b.txt", "a​b.txt", "é.txt", "per%cent.txt"} {
		rr := doMultipartUpload(srv, slug, name, []byte("hello"))
		var up struct {
			ID       string `json:"id"`
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &up); err != nil || up.ID == "" {
			t.Fatalf("%q: upload %d %s", name, rr.Code, rr.Body.String())
		}
		req := httptest.NewRequest("GET", "/api/v1/workspaces/"+slug+"/attachments/"+up.ID, nil)
		req.RemoteAddr = "127.0.0.1:1"
		dr := httptest.NewRecorder()
		srv.ServeHTTP(dr, req)
		_, params, err := mime.ParseMediaType(dr.Header().Get("Content-Disposition"))
		if err != nil || params["filename"] != up.Filename || up.Filename != name {
			t.Errorf("sent %q, stored %q, header %q parsed to %q (%v)", name, up.Filename, dr.Header().Get("Content-Disposition"), params["filename"], err)
		}
	}
}
