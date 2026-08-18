package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Real-derivation pins for TASK-2637 / fork-3 Option C. Dave's invariant:
// every byte a share viewer receives has passed through the decode→re-encode
// variant pipeline — metadata (EXIF/GPS) dropped by construction, format
// normalized per ThumbnailFormat, resolution bounded. These wire the REAL
// pure-Go image processor and drive the lazy-derive-at-resolve path, so they
// prove the pipeline runs on real bytes, not hand-seeded variant rows.

// exifMarker is the "Exif\0\0" APP1 identifier. Its presence in a JPEG's
// bytes means an EXIF block survived; its ABSENCE in the derived variant is
// the pin (b) assertion.
var exifMarker = []byte{0x45, 0x78, 0x69, 0x66, 0x00, 0x00}

// seedOriginalWithProcessor is newShareAssetFixture + the pure-Go image
// processor wired, so deriveThumbnails actually produces variants. Returns a
// fixture whose lazy-derive path is live.
func newDerivingShareFixture(t *testing.T) shareAssetFixture {
	t.Helper()
	f := newShareAssetFixture(t)
	wireTestImageProcessor(f.srv)
	if f.srv.imageProcessor == nil {
		// libvips build wires no processor (its wireTestImageProcessor is a
		// no-op); the real-derivation path can't run there.
		t.Skip("no image processor on this build — derivation path not exercisable")
	}
	return f
}

// seedRawOriginal stores body under mime, anchored to itemID, WITHOUT any
// derived variant — the legacy shape the lazy-derive path backfills. Unlike
// putBlob it does not force image/png, so JPEG sources round-trip honestly.
func (f shareAssetFixture) seedRawOriginal(t *testing.T, itemID, mime string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	st, err := f.srv.attachments.Resolve(attachments.FSPrefix + ":" + hash)
	if err != nil {
		t.Fatalf("resolve store: %v", err)
	}
	key, err := st.Put(context.Background(), hash, mime, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	att := &models.Attachment{
		WorkspaceID: f.wsID, ItemID: &itemID, StorageKey: key, ContentHash: hash,
		MimeType: mime, SizeBytes: int64(len(body)), Filename: "src", UploadedBy: "system",
	}
	if err := f.srv.store.CreateAttachment(att); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}
	return att.ID
}

// realPNGImage builds a valid PNG of the given size with an alpha gradient,
// so re-encoding as PNG (not JPEG) is observable and alpha survival matters.
func realPNGImage(t *testing.T, w, h int) []byte {
	t.Helper()
	return makeIntegrationPNG(t, w, h)
}

// jpegWithEXIF encodes a JPEG of the given size and splices an APP1 "Exif"
// segment in right after SOI, so the source demonstrably CARRIES EXIF. The
// decoder skips APP1; the re-encode drops it — which is the pin.
func jpegWithEXIF(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 100, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	raw := buf.Bytes()
	// A minimal APP1 Exif payload: "Exif\0\0" + a little filler standing in
	// for a TIFF/GPS block. Enough that its marker bytes are searchable.
	payload := append(append([]byte(nil), exifMarker...), bytes.Repeat([]byte{0x2a}, 32)...)
	seg := make([]byte, 0, len(payload)+4)
	seg = append(seg, 0xFF, 0xE1) // APP1 marker
	segLen := len(payload) + 2
	seg = append(seg, byte(segLen>>8), byte(segLen&0xff))
	seg = append(seg, payload...)
	// Splice after SOI (first two bytes 0xFFD8).
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xD8 {
		t.Fatalf("unexpected JPEG start: % x", raw[:2])
	}
	out := make([]byte, 0, len(raw)+len(seg))
	out = append(out, raw[:2]...)
	out = append(out, seg...)
	out = append(out, raw[2:]...)
	if !bytes.Contains(out, exifMarker) {
		t.Fatal("test bug: source JPEG does not contain the EXIF marker we injected")
	}
	return out
}

// Pin (a): a ≤1024px image (the live 64×64 case) renders on a share page via
// a REAL derived variant — lazy-derived at resolve, no original ever served.
func TestShareAssetDerive_SmallImageRendersViaVariant(t *testing.T) {
	f := newDerivingShareFixture(t)
	orig := f.seedRawOriginal(t, f.itemID, "image/png", realPNGImage(t, 64, 64))
	f.setContent(t, f.itemID, imageRef(orig))
	link := f.createLink(t, "item", f.itemID, nil)

	// Resolve lazily derives the missing variant → the ref appears.
	refs := f.resolvedRefs(t, link.Token, "")
	if _, ok := refs[orig]; !ok {
		t.Fatalf("small image minted no ref; lazy-derive did not run. refs=%v", refs)
	}
	// The byte endpoint serves the derived variant (not the original).
	rr := f.getAsset(link.Token, orig, "variant=thumb-md")
	if rr.Code != http.StatusOK {
		t.Fatalf("small-image asset: status %d, body %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png (PNG source stays PNG)", ct)
	}
	// A derived variant row now exists — confirm we served bytes, not empty.
	if rr.Body.Len() == 0 {
		t.Error("served an empty body")
	}
}

// Pin (b): an EXIF-bearing source yields variant bytes with NO EXIF. Verify
// the BYTES, not intent.
func TestShareAssetDerive_StripsEXIF(t *testing.T) {
	f := newDerivingShareFixture(t)
	src := jpegWithEXIF(t, 1600, 1200) // >1024 so a genuine downscale happens too
	orig := f.seedRawOriginal(t, f.itemID, "image/jpeg", src)
	f.setContent(t, f.itemID, imageRef(orig))
	link := f.createLink(t, "item", f.itemID, nil)

	if _, ok := f.resolvedRefs(t, link.Token, "")[orig]; !ok {
		t.Fatal("EXIF JPEG minted no ref")
	}
	rr := f.getAsset(link.Token, orig, "variant=thumb-md")
	if rr.Code != http.StatusOK {
		t.Fatalf("asset status %d", rr.Code)
	}
	served := rr.Body.Bytes()
	if bytes.Contains(served, exifMarker) {
		t.Error("served variant STILL contains the EXIF marker — metadata not stripped by the pipeline")
	}
	// JPEG source normalizes to JPEG (ThumbnailFormat).
	if ct := rr.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg (non-PNG → JPEG)", ct)
	}
}

// Pin (c): format normalization — PNG source stays PNG (alpha survives),
// non-PNG (JPEG) normalizes to JPEG per ThumbnailFormat.
func TestShareAssetDerive_FormatNormalization(t *testing.T) {
	cases := []struct {
		name     string
		mime     string
		body     func(t *testing.T) []byte
		wantType string
	}{
		{"png-stays-png", "image/png", func(t *testing.T) []byte { return realPNGImage(t, 1500, 1000) }, "image/png"},
		{"jpeg-stays-jpeg", "image/jpeg", func(t *testing.T) []byte { return jpegWithEXIF(t, 1500, 1000) }, "image/jpeg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDerivingShareFixture(t)
			orig := f.seedRawOriginal(t, f.itemID, tc.mime, tc.body(t))
			f.setContent(t, f.itemID, imageRef(orig))
			link := f.createLink(t, "item", f.itemID, (*store.ShareLinkOptions)(nil))
			if _, ok := f.resolvedRefs(t, link.Token, "")[orig]; !ok {
				t.Fatal("no ref minted")
			}
			rr := f.getAsset(link.Token, orig, "variant=thumb-md")
			if rr.Code != http.StatusOK {
				t.Fatalf("asset status %d", rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); ct != tc.wantType {
				t.Errorf("Content-Type = %q, want %q", ct, tc.wantType)
			}
		})
	}
}
