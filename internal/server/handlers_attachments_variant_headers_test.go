package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2964 — the byte endpoint must SAY which variant it served and whether any
// derived variant exists.
//
// The fallback at the top of the variant block is silent by design: asking for
// `thumb-md` when no such row exists serves the ORIGINAL and returns 200. That
// is correct for bytes and useless for the caller, because "here is your
// thumbnail" and "here is the original, this build could derive nothing" were
// indistinguishable on the wire. A pure-Go build derives no HEIC thumbnail, so
// the editor embedded an <img> pointed at HEIC bytes and Chrome/Firefox drew
// the broken-image icon.
//
// These legs pin the two headers the embed decision reads. They reuse
// mimeDispoFixture / seedStoredRow / serveAttachment from
// handlers_attachments_download_mime_test.go — same package, same shapes.

func TestDownloadAttachment_ServedVariantHeader(t *testing.T) {
	srv, slug, wsID, itemID := mimeDispoFixture(t)
	orig := seedStoredRow(t, srv, wsID, itemID, "image/png", "shot.png", []byte("PNGBYTES"), "", "")

	t.Run("no variant requested names the original", func(t *testing.T) {
		rr := serveAttachment(t, srv, slug, orig, "GET", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("X-Pad-Attachment-Variant"); got != models.AttachmentVariantOriginal {
			t.Errorf("X-Pad-Attachment-Variant = %q, want %q", got, models.AttachmentVariantOriginal)
		}
	})

	t.Run("a variant that does NOT exist still names the ORIGINAL, because that is what the bytes are", func(t *testing.T) {
		// The whole defect in one assertion: the status is 200 and the body is
		// the original. Naming the REQUESTED variant here would be a lie that
		// reintroduces the bug at a different layer.
		rr := serveAttachment(t, srv, slug, orig, "GET", models.AttachmentVariantThumbMd)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if got := rr.Header().Get("X-Pad-Attachment-Variant"); got != models.AttachmentVariantOriginal {
			t.Errorf("X-Pad-Attachment-Variant = %q, want %q (the fallback served the original)",
				got, models.AttachmentVariantOriginal)
		}
	})

	t.Run("a variant that DOES exist is named", func(t *testing.T) {
		seedStoredRow(t, srv, wsID, itemID, "image/jpeg", "shot-md.jpg",
			[]byte("JPEGBYTES"), orig, models.AttachmentVariantThumbMd)
		rr := serveAttachment(t, srv, slug, orig, "GET", models.AttachmentVariantThumbMd)
		if got := rr.Header().Get("X-Pad-Attachment-Variant"); got != models.AttachmentVariantThumbMd {
			t.Errorf("X-Pad-Attachment-Variant = %q, want %q", got, models.AttachmentVariantThumbMd)
		}
	})
}

func TestDownloadAttachment_DerivedHeader(t *testing.T) {
	t.Run("none when this build derived nothing", func(t *testing.T) {
		srv, slug, wsID, itemID := mimeDispoFixture(t)
		// A HEIC on a build whose processor cannot decode it: the row exists,
		// no variant rows were ever written. This is the state the bug lives in.
		heic := seedStoredRow(t, srv, wsID, itemID, "image/heic", "beach.heic", []byte("HEICBYTES"), "", "")

		rr := serveAttachment(t, srv, slug, heic, "HEAD", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		// A SENTINEL, not an empty value: absence must keep meaning "server
		// predates BUG-2964", because a client that reads absence as "no
		// variants" would turn every embed into a chip against an older build.
		if got := rr.Header().Get("X-Pad-Attachment-Derived"); got != "none" {
			t.Errorf("X-Pad-Attachment-Derived = %q, want %q", got, "none")
		}
	})

	t.Run("lists the variants that exist", func(t *testing.T) {
		srv, slug, wsID, itemID := mimeDispoFixture(t)
		orig := seedStoredRow(t, srv, wsID, itemID, "image/png", "shot.png", []byte("PNGBYTES"), "", "")
		seedStoredRow(t, srv, wsID, itemID, "image/jpeg", "sm.jpg", []byte("SM"), orig, models.AttachmentVariantThumbSm)
		seedStoredRow(t, srv, wsID, itemID, "image/jpeg", "md.jpg", []byte("MD"), orig, models.AttachmentVariantThumbMd)

		rr := serveAttachment(t, srv, slug, orig, "HEAD", "")
		if got := rr.Header().Get("X-Pad-Attachment-Derived"); got != "thumb-sm,thumb-md" {
			t.Errorf("X-Pad-Attachment-Derived = %q, want %q", got, "thumb-sm,thumb-md")
		}
	})

	t.Run("absent on the variant path, which does not answer this question", func(t *testing.T) {
		// The availability lookup is deliberately confined to the no-variant
		// path — the editor's metadata probe. The hot `?variant=thumb-md` image
		// path pays nothing, and a header there would be answering a question
		// nobody asked at the cost of two queries per image byte-serve.
		srv, slug, wsID, itemID := mimeDispoFixture(t)
		orig := seedStoredRow(t, srv, wsID, itemID, "image/png", "shot.png", []byte("PNGBYTES"), "", "")
		rr := serveAttachment(t, srv, slug, orig, "GET", models.AttachmentVariantThumbMd)
		if got := rr.Header().Get("X-Pad-Attachment-Derived"); got != "" {
			t.Errorf("X-Pad-Attachment-Derived = %q on the variant path, want it unset", got)
		}
	})
}
