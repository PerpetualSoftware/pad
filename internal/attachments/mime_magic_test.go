package attachments

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestBUG2963FormatsReachTheAllowlist is the headline property: for each
// format BUG-2963 measured as unreachable, a real file of it now sniffs as the
// spelling the allowlist uses and is ACCEPTED and stored under that spelling.
//
// Both halves are asserted deliberately. The sniff alone is not the claim —
// the claim is what the door does — and the two can diverge, which is exactly
// how .mkv hid: it sniffed to something the allowlist accepted, so the upload
// succeeded while the stored type was false.
func TestBUG2963FormatsReachTheAllowlist(t *testing.T) {
	cases := []struct {
		fixture  string
		filename string
		want     string // what the door stores
		// sniffWant is what SniffMIME alone returns, when that differs from
		// what the door stores. It differs for exactly one format: raw AAC is
		// resolved in ValidateUpload, because its signature is too weak to act
		// on without the filename and SniffMIME never sees one.
		sniffWant string
		cat       Category
		why       string
	}{
		{"tar.head512", "archive.tar", "application/x-tar", "", CategoryArchive, "ustar at offset 257"},
		{"bzip2.head512", "notes.txt.bz2", "application/x-bzip2", "", CategoryArchive, "BZh + block-size digit"},
		{"sevenzip.head512", "archive.7z", "application/x-7z-compressed", "", CategoryArchive, "six-byte 7z signature"},
		{"flac.head512", "track.flac", "audio/flac", "", CategoryAudio, "fLaC stream marker"},
		{"aac-adts.head512", "track.aac", "audio/aac", "application/octet-stream", CategoryAudio, "ADTS sync, gated on the extension"},
		{"avi.head512", "clip.avi", "video/x-msvideo", "", CategoryVideo, "video/avi alias"},
		{"ogg-opus.head512", "track.ogg", "audio/ogg", "", CategoryAudio, "application/ogg alias"},
		{"matroska.head512", "clip.mkv", "video/x-matroska", "", CategoryVideo, "EBML DocType matroska"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			head := fixture(t, tc.fixture)
			wantSniff := tc.sniffWant
			if wantSniff == "" {
				wantSniff = tc.want
			}
			if got := SniffMIME(head); got != wantSniff {
				t.Errorf("SniffMIME = %q, want %q (%s)", got, wantSniff, tc.why)
			}
			entry, code, err := ValidateUpload(head, tc.filename)
			if err != nil {
				t.Fatalf("ValidateUpload(%s) rejected: code=%s err=%v", tc.filename, code, err)
			}
			if entry.MIME != tc.want {
				t.Errorf("stored as %q, want %q", entry.MIME, tc.want)
			}
			if entry.Category != tc.cat {
				t.Errorf("category %q, want %q", entry.Category, tc.cat)
			}
		})
	}
}

// TestEBMLDocTypeDiscriminates is the control the Matroska case needs. A
// DocType read that returned video/x-matroska for every EBML file would pass
// the Matroska leg above and be strictly worse than the stdlib — it would
// break WebM, which worked. So the WebM fixture is asserted in the same test
// as the Matroska one, and the third leg pins the fail-safe: an EBML header
// carrying NEITHER string keeps the stdlib's answer rather than being refused.
func TestEBMLDocTypeDiscriminates(t *testing.T) {
	if got := SniffMIME(fixture(t, "matroska.head512")); got != "video/x-matroska" {
		t.Errorf("matroska fixture sniffed %q, want video/x-matroska", got)
	}
	if got := SniffMIME(fixture(t, "webm.head512")); got != "video/webm" {
		t.Errorf("webm fixture sniffed %q, want video/webm — the DocType read must not "+
			"claim Matroska for every EBML file", got)
	}

	// EBML magic, then bytes that are neither DocType. Synthetic on purpose:
	// no muxer emits this, and the property under test is what the code does
	// when its window comes up empty, not what any encoder writes.
	blank := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 120)...)
	if got := SniffMIME(blank); got != "video/webm" {
		t.Errorf("EBML header with no DocType sniffed %q, want the stdlib's video/webm — "+
			"an unrecognised DocType must fall back, not refuse", got)
	}
}

// TestADTSNeedsBytesAndExtension pins both halves of the one extension-gated
// signature. Each leg fails if either half is dropped, which is the point: the
// gate is what makes a twelve-bit signature safe to act on.
func TestADTSNeedsBytesAndExtension(t *testing.T) {
	adts := fixture(t, "aac-adts.head512")

	if _, _, err := ValidateUpload(adts, "track.aac"); err != nil {
		t.Fatalf("ADTS bytes named .aac were rejected: %v", err)
	}

	// Bytes without the extension: the sync alone must not introduce a type.
	if _, code, err := ValidateUpload(adts, "track.bin"); err == nil {
		t.Error("ADTS bytes named .bin were accepted; the extension gate is not applied")
	} else if code != "mime_not_allowed" {
		t.Errorf("code = %q, want mime_not_allowed", code)
	}

	// Extension without the bytes: the name alone must not introduce a type.
	notADTS := make([]byte, 512) // all zeroes: application/octet-stream
	if _, code, err := ValidateUpload(notADTS, "track.aac"); err == nil {
		t.Error("non-ADTS bytes named .aac were accepted; the extension is being trusted alone")
	} else if code != "mime_not_allowed" {
		t.Errorf("code = %q, want mime_not_allowed", code)
	}

	// A type the stdlib DOES recognise is untouched by the .aac name — the
	// branch only runs on application/octet-stream, so this stays an image
	// and is refused for the category mismatch it is.
	png := []byte("\x89PNG\r\n\x1a\n")
	if _, code, _ := ValidateUpload(png, "sneaky.aac"); code != "mime_extension_mismatch" {
		t.Errorf("PNG bytes named .aac gave code %q, want mime_extension_mismatch", code)
	}
}

// TestOpaqueMagicDoesNotOverrideTheStdlib pins the ordering that makes the
// magic table safe: it is consulted only where the stdlib returned
// application/octet-stream, so it can add a detection and never replace one.
//
// The input is spliced rather than encoder-produced, because the property is
// about the ORDER of two checks and no real file exercises it — a genuine PNG
// has no reason to carry `ustar` at offset 257. Delete the
// application/octet-stream case in SniffMIME and this test fails while every
// other test in this file still passes.
func TestOpaqueMagicDoesNotOverrideTheStdlib(t *testing.T) {
	buf := make([]byte, 512)
	copy(buf, []byte("\x89PNG\r\n\x1a\n"))
	copy(buf[257:], []byte("ustar"))

	if got := sniffOpaqueMagic(buf); got != "application/x-tar" {
		t.Fatalf("premise failed: sniffOpaqueMagic = %q, want application/x-tar — "+
			"this test asserts the tar magic LOSES, so it must first be present", got)
	}
	if got := SniffMIME(buf); got != "image/png" {
		t.Errorf("SniffMIME = %q, want image/png — a magic pre-check must not "+
			"override a type the stdlib recognised", got)
	}
}

// TestBUG2963F6RemovedSpellings covers the deletions and, more importantly,
// the thing deleting them nearly broke. extMIMEMap's values are looked up in
// `allowed`, and an extension whose value is NOT allowed is the mechanism that
// refuses .svg and .exe — so removing application/xml while .xml still named
// it would have turned every XML upload into extension_blocked. The upload leg
// is the regression guard; the lookup legs are the deletion itself.
func TestBUG2963F6RemovedSpellings(t *testing.T) {
	for _, m := range []string{"application/javascript", "text/yaml", "application/xml"} {
		if _, ok := LookupMIME(m); ok {
			t.Errorf("%q is still on the allowlist; BUG-2963 F6 removed it as unreachable", m)
		}
	}

	xml := []byte(`<?xml version="1.0" encoding="UTF-8"?><root><item ref="BUG-2963"/></root>`)
	entry, code, err := ValidateUpload(xml, "data.xml")
	if err != nil {
		t.Fatalf(".xml upload rejected after the F6 deletions: code=%s err=%v", code, err)
	}
	if entry.MIME != "text/xml" {
		t.Errorf(".xml stored as %q, want text/xml", entry.MIME)
	}

	// audio/webm is unreachable too and STAYS, by ruling: separating it from
	// video/webm needs a track read, nobody has asked, and an unreachable
	// entry costs nothing until someone reads the map. Asserted so a later
	// tidy-up of "unreachable entries" has to meet the ruling first.
	if _, ok := LookupMIME("audio/webm"); !ok {
		t.Error("audio/webm was removed; BUG-2963 F6 ruled it stays, unreachable, with its comment")
	}
}

// TestOpaqueMagicNegatives pins the tightenings inside the magic table, each
// of which is invisible to the positive cases above — those pass whether or
// not the extra condition is there.
func TestOpaqueMagicNegatives(t *testing.T) {
	pad := func(b []byte) []byte {
		out := make([]byte, 512)
		copy(out, b)
		return out
	}
	cases := []struct {
		name string
		in   []byte
		why  string
	}{
		{"BZh without the block-size digit", pad([]byte("BZhX and then some text")),
			"the digit is what makes an accidental match need four bytes, not three"},
		{"BZh followed by a zero digit", pad([]byte("BZh0")),
			"bzip2 block sizes are 1..9; '0' is not one"},
		{"ustar at 256 rather than 257", func() []byte {
			b := make([]byte, 512)
			copy(b[256:], []byte("ustar"))
			return b
		}(), "the tar magic is at a fixed offset; one byte off is not a tar"},
		{"fLaC not at the start", pad([]byte("\x00\x00fLaC")),
			"the FLAC marker opens the stream; anywhere else it is just bytes"},
		{"truncated 7z signature", pad([]byte{0x37, 0x7A, 0xBC, 0xAF, 0x27}),
			"five of the six signature bytes is not the signature"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniffOpaqueMagic(tc.in); got != "" {
				t.Errorf("sniffOpaqueMagic = %q, want no match — %s", got, tc.why)
			}
		})
	}
}
