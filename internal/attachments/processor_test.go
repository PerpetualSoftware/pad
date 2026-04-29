//go:build !libvips

package attachments

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// makeTestImage returns a freshly-allocated NRGBA image filled with a
// distinguishable pattern so transformation tests can assert on
// specific pixels. Width/height are exported so callers can dial them
// up for memory-pressure tests without having to know the impl.
func makeTestImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{
				R: uint8(x % 256),
				G: uint8(y % 256),
				B: uint8((x + y) % 256),
				A: 255,
			})
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test JPEG: %v", err)
	}
	return buf.Bytes()
}

func encodeGIF(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode test GIF: %v", err)
	}
	return buf.Bytes()
}

func TestProcessor_Capabilities_PureGo(t *testing.T) {
	p := NewProcessor()
	caps := p.Capabilities()

	wantFormats := []string{"png", "jpeg", "gif", "bmp", "tiff"}
	if len(caps.ImageFormats) != len(wantFormats) {
		t.Errorf("ImageFormats = %v, want %v", caps.ImageFormats, wantFormats)
	}
	got := strings.Join(caps.ImageFormats, ",")
	want := strings.Join(wantFormats, ",")
	if got != want {
		t.Errorf("ImageFormats = %q, want %q", got, want)
	}
	if !caps.CanTranscode {
		t.Error("CanTranscode = false, want true (pure-Go can encode PNG + JPEG)")
	}
	if caps.MaxPixels != MaxPixelsDefault {
		t.Errorf("MaxPixels = %d, want %d", caps.MaxPixels, MaxPixelsDefault)
	}
}

func TestProcessor_Decode_RoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		format string
		bytes  func(t *testing.T) []byte
	}{
		{"png", "png", func(t *testing.T) []byte { return encodePNG(t, makeTestImage(64, 32)) }},
		{"jpeg", "jpeg", func(t *testing.T) []byte { return encodeJPEG(t, makeTestImage(64, 32)) }},
		{"gif", "gif", func(t *testing.T) []byte { return encodeGIF(t, makeTestImage(64, 32)) }},
	}
	p := NewProcessor()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			img, format, err := p.Decode(bytes.NewReader(c.bytes(t)))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if format != c.format {
				t.Errorf("format = %q, want %q", format, c.format)
			}
			if img.Bounds().Dx() != 64 || img.Bounds().Dy() != 32 {
				t.Errorf("decoded bounds = %v, want 64x32", img.Bounds())
			}
		})
	}
}

func TestProcessor_Decode_RejectsUnsupportedFormat(t *testing.T) {
	// Synthetic "WebP-ish" bytes — RIFF header but not actually a valid
	// WebP. image.DecodeConfig will fail because no WebP decoder is
	// registered in the pure-Go build.
	bogus := []byte("RIFF\x10\x00\x00\x00WEBPVP8L\x00\x00\x00\x00")
	_, _, err := NewProcessor().Decode(bytes.NewReader(bogus))
	if err == nil {
		t.Fatal("Decode succeeded on bogus WebP-like bytes; want ErrUnsupportedFormat")
	}
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("err = %v, want errors.Is ErrUnsupportedFormat", err)
	}
}

func TestProcessor_Decode_RejectsTooLarge(t *testing.T) {
	// Build a real 1x1 PNG, patch its IHDR to claim absurd dimensions,
	// and recompute the chunk's CRC32 so image.DecodeConfig can read
	// the patched header and our MaxPixels gate fires before any real
	// pixel buffer gets allocated. We can't construct an actual
	// 100k x 100k PNG — that's the entire point of the header-peek
	// check, which exists so an attacker can't OOM the server with a
	// forged dimensions claim.
	//
	// PNG layout: 8-byte signature, then chunks. Each chunk:
	//   length(4) type(4) data(N) crc(4)
	// IHDR is the first chunk and is always 13 bytes of data.
	// CRC covers type + data — i.e. bytes [12 : 12+4+13] = [12 : 29].
	template := encodePNG(t, makeTestImage(1, 1))
	if len(template) < 33 {
		t.Fatalf("PNG template too short: %d bytes", len(template))
	}
	patched := append([]byte(nil), template...)
	const huge = uint32(100000) // 100k x 100k = 1e10 px > MaxPixelsDefault (64M)
	binary.BigEndian.PutUint32(patched[16:20], huge) // width
	binary.BigEndian.PutUint32(patched[20:24], huge) // height
	crc := crc32.ChecksumIEEE(patched[12:29])        // type + 13-byte data
	binary.BigEndian.PutUint32(patched[29:33], crc)

	_, _, err := NewProcessor().Decode(bytes.NewReader(patched))
	if err == nil {
		t.Fatal("Decode succeeded on 100kx100k claim; want ErrImageTooLarge")
	}
	if !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("err = %v, want errors.Is ErrImageTooLarge", err)
	}
}

func TestProcessor_Resize_PreservesAspectAndShorterEdgePassThrough(t *testing.T) {
	p := NewProcessor()
	src := makeTestImage(800, 400)

	// Resize where source > target → scaled
	out, err := p.Resize(src, 256)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if out.Bounds().Dx() != 256 {
		t.Errorf("scaled width = %d, want 256", out.Bounds().Dx())
	}
	// Aspect 2:1 → height 128
	if out.Bounds().Dy() != 128 {
		t.Errorf("scaled height = %d, want 128 (preserve 2:1 aspect)", out.Bounds().Dy())
	}

	// Resize where source already smaller → pass-through (same image)
	small := makeTestImage(100, 50)
	out2, err := p.Resize(small, 1024)
	if err != nil {
		t.Fatalf("Resize pass-through: %v", err)
	}
	if out2.Bounds().Dx() != 100 || out2.Bounds().Dy() != 50 {
		t.Errorf("pass-through bounds = %v, want 100x50", out2.Bounds())
	}
}

func TestProcessor_Resize_TallerThanWide(t *testing.T) {
	p := NewProcessor()
	src := makeTestImage(400, 800) // 1:2

	out, err := p.Resize(src, 256)
	if err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if out.Bounds().Dy() != 256 {
		t.Errorf("scaled height = %d, want 256", out.Bounds().Dy())
	}
	if out.Bounds().Dx() != 128 {
		t.Errorf("scaled width = %d, want 128 (preserve 1:2 aspect)", out.Bounds().Dx())
	}
}

func TestProcessor_Rotate(t *testing.T) {
	p := NewProcessor()
	src := makeTestImage(64, 32)

	cases := []struct {
		deg          int
		wantW, wantH int
	}{
		{0, 64, 32},
		{90, 32, 64},
		{180, 64, 32},
		{270, 32, 64},
		{-90, 32, 64}, // negative normalizes to 270
		{360, 64, 32}, // 360 normalizes to 0
		{450, 32, 64}, // 450 normalizes to 90
	}
	for _, c := range cases {
		out, err := p.Rotate(src, c.deg)
		if err != nil {
			t.Errorf("Rotate(%d): %v", c.deg, err)
			continue
		}
		if out.Bounds().Dx() != c.wantW || out.Bounds().Dy() != c.wantH {
			t.Errorf("Rotate(%d) bounds = %v, want %dx%d", c.deg, out.Bounds(), c.wantW, c.wantH)
		}
	}
}

func TestProcessor_Rotate_RejectsNon90Multiple(t *testing.T) {
	if _, err := NewProcessor().Rotate(makeTestImage(8, 8), 45); err == nil {
		t.Error("Rotate(45) succeeded; want error (only multiples of 90 supported)")
	}
}

func TestProcessor_Crop(t *testing.T) {
	p := NewProcessor()
	src := makeTestImage(100, 100)

	out, err := p.Crop(src, image.Rect(10, 20, 60, 80))
	if err != nil {
		t.Fatalf("Crop: %v", err)
	}
	if out.Bounds().Dx() != 50 || out.Bounds().Dy() != 60 {
		t.Errorf("Crop bounds = %v, want 50x60", out.Bounds())
	}
}

func TestProcessor_Crop_ClipsToImageBounds(t *testing.T) {
	p := NewProcessor()
	src := makeTestImage(100, 100)

	// Rect extends past image bounds — should clip, not error.
	out, err := p.Crop(src, image.Rect(50, 50, 200, 200))
	if err != nil {
		t.Fatalf("Crop with overflow rect: %v", err)
	}
	if out.Bounds().Dx() != 50 || out.Bounds().Dy() != 50 {
		t.Errorf("clipped bounds = %v, want 50x50", out.Bounds())
	}
}

func TestProcessor_Crop_RejectsEmptyIntersection(t *testing.T) {
	p := NewProcessor()
	src := makeTestImage(100, 100)
	if _, err := p.Crop(src, image.Rect(200, 200, 300, 300)); err == nil {
		t.Error("Crop with rect outside bounds succeeded; want error")
	}
}

func TestProcessor_Encode_PNGAndJPEG(t *testing.T) {
	p := NewProcessor()
	src := makeTestImage(64, 32)

	for _, format := range []string{"png", "jpeg"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			if err := p.Encode(src, format, &buf); err != nil {
				t.Fatalf("Encode(%s): %v", format, err)
			}
			// Round-trip: decode the encoded bytes; format must match.
			_, gotFormat, err := image.Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("decode encoded %s: %v", format, err)
			}
			if gotFormat != format {
				t.Errorf("decoded format = %q, want %q", gotFormat, format)
			}
		})
	}
}

func TestProcessor_Encode_RejectsUnknownFormat(t *testing.T) {
	p := NewProcessor()
	var buf bytes.Buffer
	err := p.Encode(makeTestImage(8, 8), "webp", &buf)
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("Encode(webp) err = %v, want errors.Is ErrUnsupportedFormat", err)
	}
}

func TestThumbnailFormat(t *testing.T) {
	cases := map[string]string{
		"png":  "png",
		"jpeg": "jpeg",
		"gif":  "jpeg",
		"bmp":  "jpeg",
		"tiff": "jpeg",
		"":     "jpeg",
	}
	for in, want := range cases {
		if got := ThumbnailFormat(in); got != want {
			t.Errorf("ThumbnailFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestThumbnailMimeAndExt(t *testing.T) {
	if m := ThumbnailMime("png"); m != "image/png" {
		t.Errorf("ThumbnailMime(png) = %q", m)
	}
	if m := ThumbnailMime("jpeg"); m != "image/jpeg" {
		t.Errorf("ThumbnailMime(jpeg) = %q", m)
	}
	if e := ThumbnailExt("png"); e != ".png" {
		t.Errorf("ThumbnailExt(png) = %q", e)
	}
	if e := ThumbnailExt("jpeg"); e != ".jpg" {
		t.Errorf("ThumbnailExt(jpeg) = %q", e)
	}
}
