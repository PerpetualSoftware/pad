package attachments

import (
	"encoding/binary"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// readFixture loads one of the real encoder outputs described in
// testdata/README.md. Every ISO-BMFF acceptance assertion in this file runs on
// those bytes rather than on a header typed from the spec: the claim is "what
// encoders emit is recognised", and a hand-built fixture would only test this
// package against my reading of ISO/IEC 23008-12.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestSniffMIME_RealISOBMFFStillImages is BUG-2961's regression test: before
// the ftyp pre-check, every one of these sniffed as application/octet-stream
// and was refused 415 by an allowlist that names all three types.
func TestSniffMIME_RealISOBMFFStillImages(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
		note    string
	}{
		{"apple-still-heic.head512", "image/heic", "sips HEIC — major brand heic"},
		{"apple-still.avif", "image/avif", "sips AVIF — major brand avif"},
		{"still.avif", "image/avif", "libheif/aom AVIF — a second encoder, same major brand"},
		{"still-mif1.heif", "image/heif", "libheif — major brand mif1, no codec-specific brand"},
	}
	for _, tc := range cases {
		body := readFixture(t, tc.fixture)
		if got := SniffMIME(body); got != tc.want {
			t.Errorf("SniffMIME(%s) = %q, want %q (%s)", tc.fixture, got, tc.want, tc.note)
		}
		entry, code, err := ValidateUpload(body, "photo"+canonicalExtOrDie(t, tc.want))
		if err != nil {
			t.Errorf("ValidateUpload(%s) rejected (code=%s): %v", tc.fixture, code, err)
			continue
		}
		if entry.MIME != tc.want || entry.Category != CategoryImage {
			t.Errorf("ValidateUpload(%s) = {%s, %s}, want {%s, image}", tc.fixture, entry.MIME, entry.Category, tc.want)
		}
	}
}

func canonicalExtOrDie(t *testing.T, mime string) string {
	t.Helper()
	ext := ExtensionForMIME(mime)
	if ext == "" {
		t.Fatalf("no canonical extension for %q", mime)
	}
	return ext
}

// TestValidateUpload_ISOBMFFDoesNotBecomeExtensionTrust is the row the reporter
// asked to keep whatever the fix turned out to be: the OTHER way to make the
// three allowlist entries reachable is to trust the filename for them (the
// escape hatch ValidateUpload already grants zip-based Office documents), and
// under that fix a renamed file passes on its name alone. These four cases all
// fail under extension-trust and pass under a bytes-first sniff.
func TestValidateUpload_ISOBMFFDoesNotBecomeExtensionTrust(t *testing.T) {
	heic := readFixture(t, "apple-still-heic.head512")

	// 1. Non-image bytes wearing an image extension: still refused, and refused
	//    on the SNIFF (mime_not_allowed), not on the name.
	for _, name := range []string{"totally-a-photo.heic", "totally-a-photo.avif", "totally-a-photo.heif"} {
		_, code, err := ValidateUpload(exeHeader, name)
		if err == nil {
			t.Errorf("ValidateUpload(exe bytes, %q) accepted; want refusal", name)
			continue
		}
		if code != "mime_not_allowed" {
			t.Errorf("ValidateUpload(exe bytes, %q) code = %q, want mime_not_allowed", name, code)
		}
	}

	// 2. The reporter's fourth row, inverted twice. Real HEIC bytes named .jpg
	//    are accepted as image/heic — the sniff decides the stored type, and
	//    both are images, so the category cross-check has nothing to object to.
	entry, code, err := ValidateUpload(heic, "photo.jpg")
	if err != nil {
		t.Fatalf("ValidateUpload(HEIC bytes, photo.jpg) refused (code=%s): %v", code, err)
	}
	if entry.MIME != "image/heic" {
		t.Errorf("ValidateUpload(HEIC bytes, photo.jpg) = %q, want image/heic (the BYTES decide)", entry.MIME)
	}

	// 3. Real HEIC bytes named .exe are still refused by the extension
	//    blocklist — the bytes-first sniff does not disarm that gate.
	if _, code, err := ValidateUpload(heic, "photo.exe"); err == nil {
		t.Error("ValidateUpload(HEIC bytes, photo.exe) accepted; want extension_blocked")
	} else if code != "extension_blocked" {
		t.Errorf("ValidateUpload(HEIC bytes, photo.exe) code = %q, want extension_blocked", code)
	}
}

// TestSniffISOBMFFImage_SequenceBrandsAreNotStillImages pins the deliberate
// omission. Sequence brands name image/heic-sequence, image/avif-sequence and
// image/heif-sequence, none of which is on the allowlist, so recognising them
// here would produce a sniff result that ValidateUpload then refuses anyway —
// with a MORE confusing message. They must fall through to the stdlib.
func TestSniffISOBMFFImage_SequenceBrandsAreNotStillImages(t *testing.T) {
	for _, brand := range []string{"hevc", "hevx", "hevm", "hevs", "avis", "msf1"} {
		body := ftypBox(brand, brand, "miaf")
		if got := sniffISOBMFF(body); got != "" {
			t.Errorf("sniffISOBMFF(major=%s) = %q, want \"\" (sequence brands are not on the allowlist)", brand, got)
		}
		if _, _, err := ValidateUpload(body, "clip."+brand); err == nil {
			t.Errorf("ValidateUpload(major=%s) accepted; want refusal", brand)
		}
	}
}

// TestSniffISOBMFFImage_BrandsAnywhereInTheBox covers the shape the reporter
// flagged: on their HEIC, mif1 is a COMPATIBLE brand and heic is the major, and
// elsewhere the two swap. A check on bytes 8..12 alone would answer correctly
// for one file and wrongly for the other, so the scan covers the whole box.
func TestSniffISOBMFFImage_BrandsAnywhereInTheBox(t *testing.T) {
	cases := []struct {
		name   string
		brands []string
		want   string
	}{
		{"major heic", []string{"heic", "mif1", "miaf"}, "image/heic"},
		{"heic as a compatible brand under an unknown major", []string{"mif2", "miaf", "heic"}, "image/heic"},
		{"avif as a compatible brand under a mif1 major", []string{"mif1", "avif", "miaf"}, "image/avif"},
		{"heic as a compatible brand under a mif1 major", []string{"mif1", "miaf", "heic"}, "image/heic"},
		{"mif1 alone", []string{"mif1", "miaf"}, "image/heif"},
		{"heix / heim / heis majors", []string{"heix"}, "image/heic"},
		{"no image brand at all", []string{"isom", "iso2", "avc1"}, ""},
	}
	for _, tc := range cases {
		if got := sniffISOBMFF(ftypBox(tc.brands[0], tc.brands[1:]...)); got != tc.want {
			t.Errorf("%s: sniffISOBMFF(%v) = %q, want %q", tc.name, tc.brands, got, tc.want)
		}
	}
}

// TestSniffISOBMFFImage_YieldsToVideo keeps the "can only ADD detections"
// property honest: an MP4 the stdlib already detects must keep sniffing as
// video/mp4 even if it also carries a still-image brand.
func TestSniffISOBMFFImage_YieldsToVideo(t *testing.T) {
	plainMP4 := ftypBox("isom", "isom", "iso2", "mp41")
	if got := SniffMIME(plainMP4); got != "video/mp4" {
		t.Errorf("SniffMIME(mp4) = %q, want video/mp4", got)
	}
	mixed := ftypBox("isom", "mp42", "heic")
	if got := sniffISOBMFF(mixed); got != "" {
		t.Errorf("sniffISOBMFF(mp4 brands + heic) = %q, want \"\" (a video container stays a video)", got)
	}
}

// TestSniffISOBMFFImage_MalformedInput asserts the parser never panics and
// never guesses on input it cannot read. Every case here is something a hostile
// or truncated upload can actually produce.
func TestSniffISOBMFFImage_MalformedInput(t *testing.T) {
	full := readFixture(t, "apple-still.avif")

	cases := map[string][]byte{
		"empty":                         {},
		"shorter than the ftyp box":     full[:11],
		"not an ftyp box":               append([]byte{0, 0, 0, 32, 'm', 'o', 'o', 'v'}, full[8:64]...),
		"64-bit largesize (size 1)":     withBoxSize(full, 1),
		"box size 0 (to end of file)":   withBoxSize(full, 0),
		"box size past the buffer":      withBoxSize(full[:64], 1<<20),
		"box size not a brand multiple": withBoxSize(full, 30),
	}
	for name, body := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: sniffISOBMFF panicked: %v", name, r)
				}
			}()
			sniffISOBMFF(body)
		}()
	}

	// Size 1 is a 64-bit largesize box: the brands sit eight bytes further
	// along, so reading them at the 32-bit offsets would report a type derived
	// from the size field. It must decline rather than guess.
	if got := sniffISOBMFF(withBoxSize(full, 1)); got != "" {
		t.Errorf("sniffISOBMFF(largesize box) = %q, want \"\" (brands are shifted; offsets do not apply)", got)
	}

	// A buffer that stops inside the 16-byte ftyp header is a truncated file,
	// not a small one, and must not be classified from the fields that happen
	// to be present (codex round 2, P2).
	for n := 12; n < 16; n++ {
		body := []byte{0, 0, 0, 16}
		body = append(body, "ftyp"...)
		body = append(body, "heic"...)
		body = append(body, "\x00\x00\x00\x00"...)
		if got := sniffISOBMFF(body[:n]); got != "" {
			t.Errorf("sniffISOBMFF(%d bytes) = %q, want \"\" (the ftyp header is 16 bytes)", n, got)
		}
	}

	// A declared size of 2..15 cannot be an ftyp box — the header, major brand
	// and minor version are 16 bytes on their own — so the bytes at 8.. are not
	// brands of this box and must not be read as any (codex round 1, P2).
	// Without this the payload below, which a caller controls entirely, is
	// classified image/heic and accepted by the upload door.
	for size := uint32(2); size < 16; size++ {
		body := []byte{0, 0, 0, byte(size)}
		body = append(body, "ftyp"...)
		body = append(body, "heic"...)
		body = append(body, "mif1"...)
		if got := sniffISOBMFF(body); got != "" {
			t.Errorf("sniffISOBMFF(box size %d) = %q, want \"\" (a size below 16 is not an ftyp box)", size, got)
		}
	}

	// Size 0 is legal and means "to the end of the file": it shifts nothing, so
	// a real file carrying it still resolves.
	if got := sniffISOBMFF(withBoxSize(full, 0)); got != "image/avif" {
		t.Errorf("sniffISOBMFF(box size 0) = %q, want image/avif (0 means to end of file)", got)
	}

	// A truncated head is the ordinary case for a large ftyp box and must
	// still resolve: the major brand sits at bytes 8..12, well inside it.
	if got := sniffISOBMFF(full[:16]); got != "image/avif" {
		t.Errorf("sniffISOBMFF(first 16 bytes) = %q, want image/avif", got)
	}
	// A box size larger than the buffer must not read past it.
	if got := sniffISOBMFF(withBoxSize(full[:32], 1<<20)); got != "image/avif" {
		t.Errorf("sniffISOBMFF(oversized box size, truncated buffer) = %q, want image/avif", got)
	}
}

// TestISOBMFFBrandTableStaysOnTheAllowlist is the drift guard. A brand mapped
// to a MIME that `allowed` does not carry would make this sniffer produce a
// type ValidateUpload immediately refuses — the failure would look like the bug
// it fixed, and the confusing part would be that the sniff was right.
func TestISOBMFFBrandTableStaysOnTheAllowlist(t *testing.T) {
	mimes := []string{isoBMFFGenericImageBrandMIME}
	for _, m := range isoBMFFStillImageBrands {
		mimes = append(mimes, m)
	}
	for _, m := range mimes {
		entry, ok := LookupMIME(m)
		if !ok {
			t.Errorf("ISO-BMFF brand table maps to %q, which is NOT on the upload allowlist", m)
			continue
		}
		if entry.Category != CategoryImage {
			t.Errorf("ISO-BMFF brand table maps to %q, whose category is %q, want image", m, entry.Category)
		}
	}
}

// TestSniffISOBMFFImage_ScanStopsAtTheBox pins the box-size bound. Without it
// the scan runs past ftyp into the following boxes, where any four-aligned
// occurrence of "heic"/"avif"/"mif1" — in a payload, a filename, an item
// name — would be read as a brand the file never declared.
func TestSniffISOBMFFImage_ScanStopsAtTheBox(t *testing.T) {
	body := ftypBox("isom", "iso2")
	body = append(body, 0, 0, 0, 12)
	body = append(body, "free"...)
	body = append(body, "heic"...) // payload bytes, four-aligned, NOT a brand
	if got := sniffISOBMFF(body); got != "" {
		t.Errorf("sniffISOBMFF(non-image ftyp + \"heic\" in a later box) = %q, want \"\"", got)
	}
}

// TestSniffISOBMFFImage_MinorVersionIsNotABrand covers bytes 12..16, which are
// a version number and not part of the brand list (14496-12 §4.3). A file whose
// minor version happens to spell a brand must not be read as declaring it.
func TestSniffISOBMFFImage_MinorVersionIsNotABrand(t *testing.T) {
	body := []byte{0, 0, 0, 16}
	body = append(body, "ftyp"...)
	body = append(body, "isom"...) // major brand
	body = append(body, "avif"...) // minor VERSION, coincidentally brand-shaped
	if got := sniffISOBMFF(body); got != "" {
		t.Errorf("sniffISOBMFF(minor version = \"avif\") = %q, want \"\"", got)
	}
}

// ftypBox builds an ftyp box with the given major and compatible brands. It is
// used ONLY for shapes no encoder here can produce (sequence brands, brand
// orderings, malformed sizes) — every acceptance case runs on a real file.
func ftypBox(major string, compatible ...string) []byte {
	box := make([]byte, 0, 16+4*len(compatible)+8)
	size := 16 + 4*len(compatible)
	box = binary.BigEndian.AppendUint32(box, uint32(size))
	box = append(box, "ftyp"...)
	box = append(box, major...)
	box = append(box, 0, 0, 0, 0) // minor version
	for _, b := range compatible {
		box = append(box, b...)
	}
	// A trailing box so the buffer does not end exactly at the ftyp box —
	// real files never do.
	box = binary.BigEndian.AppendUint32(box, 8)
	box = append(box, "free"...)
	return box
}

// withBoxSize rewrites the ftyp box size field, for the malformed-input cases.
func withBoxSize(body []byte, size uint32) []byte {
	out := make([]byte, len(body))
	copy(out, body)
	binary.BigEndian.PutUint32(out[:4], size)
	return out
}

// TestBUG2963F4Brands covers the audio/video half of the ISO-BMFF pre-check:
// "qt  " -> video/quicktime, "M4A " -> audio/mp4, both from the MAJOR brand.
//
// Each leg asserts what the STANDARD LIBRARY says about the same bytes, and
// that assertion is the point rather than decoration. It is what separates the
// two brands: for "qt  " the stdlib has no opinion, so this ADDS a detection;
// for "M4A " the stdlib names video/mp4 from a compatible "mp41" brand, so
// this OVERRIDES it. A test that checked only the final answer would call
// those the same thing, and the second is the one carrying a trust decision.
func TestBUG2963F4Brands(t *testing.T) {
	t.Run("quicktime", func(t *testing.T) {
		head := readFixture(t, "quicktime.head512")

		if got := http.DetectContentType(head); NormalizeMIME(got) != "application/octet-stream" {
			t.Fatalf("premise failed: the stdlib called this %q, not application/octet-stream — "+
				"this leg exists to cover the case where nothing identified the file", got)
		}
		if got := sniffISOBMFF(head); got != "video/quicktime" {
			t.Errorf("sniffISOBMFF = %q, want video/quicktime", got)
		}
		if got := SniffMIME(head); got != "video/quicktime" {
			t.Errorf("SniffMIME = %q, want video/quicktime", got)
		}

		entry, code, err := ValidateUpload(head, "clip.mov")
		if err != nil {
			t.Fatalf("a real QuickTime file was refused (code=%s): %v — video/quicktime "+
				"has been on the allowlist the whole time", code, err)
		}
		if entry.MIME != "video/quicktime" || entry.Category != CategoryVideo {
			t.Errorf("stored %q/%v, want video/quicktime/%v", entry.MIME, entry.Category, CategoryVideo)
		}
		if !entry.ServeInline() {
			t.Error("ServeInline() = false; video/quicktime is a RenderInline entry")
		}
	})

	t.Run("m4a major brand", func(t *testing.T) {
		head := readFixture(t, "m4a-brand.head512")

		if got := NormalizeMIME(http.DetectContentType(head)); got != "video/mp4" {
			t.Fatalf("premise failed: the stdlib called this %q, not video/mp4 — this leg "+
				"exists to cover the one brand that overrides the stdlib, and without "+
				"the stdlib naming a video type there is nothing being overridden", got)
		}
		if got := sniffISOBMFF(head); got != "audio/mp4" {
			t.Errorf("sniffISOBMFF = %q, want audio/mp4 — the major brand is \"M4A \" and "+
				"says audio; the stdlib's video/mp4 comes from a COMPATIBLE mp41 brand", got)
		}
		if got := SniffMIME(head); got != "audio/mp4" {
			t.Errorf("SniffMIME = %q, want audio/mp4", got)
		}

		entry, code, err := ValidateUpload(head, "clip.m4a")
		if err != nil {
			t.Fatalf("a real M4A-branded file was refused (code=%s): %v", code, err)
		}
		if entry.MIME != "audio/mp4" || entry.Category != CategoryAudio {
			t.Errorf("stored %q/%v, want audio/mp4/%v — the CATEGORY is what this fixes; "+
				"it decides whether the UI offers an audio player or a video one",
				entry.MIME, entry.Category, CategoryAudio)
		}
		if !entry.ServeInline() {
			t.Error("ServeInline() = false; audio/mp4 is a RenderInline entry, as video/mp4 was")
		}
	})

	// The AV brands are read from the MAJOR brand and nowhere else. A file
	// whose major brand is isom and whose COMPATIBLE list carries "M4A " is
	// not answered for here — reading a category out of the compatible list
	// is a wider rule than the one that was granted, and this is the leg that
	// tells the two apart. Built rather than encoded, like the other
	// brand-ordering cases in this file.
	t.Run("compatible brands do not speak", func(t *testing.T) {
		body := ftypBox("isom", "iso2", "M4A ")
		if got := sniffISOBMFF(body); got != "" {
			t.Errorf("sniffISOBMFF(major=isom, compatible=[iso2 \"M4A \"]) = %q, want \"\" — "+
				"only the major brand may name a category", got)
		}
	})

	// The isom-branded audio file, which the brands CANNOT decide: its major
	// brand names no category and its compatible brands include mp41, so the
	// pre-check yields and the stdlib's video/mp4 stands. This is the premise
	// of the extension-trust leg in mime_test.go, asserted here so that leg's
	// starting point is a measured fact rather than a claim in a comment.
	t.Run("isom brand yields", func(t *testing.T) {
		head := readFixture(t, "m4a-isom.head512")

		if got := sniffISOBMFF(head); got != "" {
			t.Errorf("sniffISOBMFF = %q, want \"\" — an isom major brand names no category, "+
				"and reading one out of the compatible list is the track read F4 does not do", got)
		}
		if got := SniffMIME(head); got != "video/mp4" {
			t.Errorf("SniffMIME = %q, want video/mp4 (the stdlib's answer, from the mp41 "+
				"compatible brand)", got)
		}
	})
}
