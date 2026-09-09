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
		// resolved in ValidateUpload, because its structure is too small to
		// act on without the filename and SniffMIME never sees one.
		sniffWant string
		cat       Category
		why       string
	}{
		{"tar.head512", "archive.tar", "application/x-tar", "", CategoryArchive, "ustar at 257 plus a verified header checksum"},
		{"bzip2.head512", "notes.txt.bz2", "application/x-bzip2", "", CategoryArchive, "BZh, block-size digit, block magic"},
		{"sevenzip.head512", "archive.7z", "application/x-7z-compressed", "", CategoryArchive, "signature plus start-header CRC"},
		{"flac.head512", "track.flac", "audio/flac", "", CategoryAudio, "fLaC plus a 34-byte STREAMINFO block"},
		{"aac-adts.head512", "track.aac", "audio/aac", "application/octet-stream", CategoryAudio, "valid ADTS header, gated on the extension"},
		{"avi.head512", "clip.avi", "video/x-msvideo", "", CategoryVideo, "video/avi alias"},
		{"ogg-opus.head512", "track.ogg", "audio/ogg", "", CategoryAudio, "Ogg page whose first packet is OpusHead"},
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

// TestStructuralValidationRefusesNearMisses is the test this file most needed
// and did not have. Every input below carries the format's MAGIC and fails its
// STRUCTURE, and every one was ACCEPTED by the first version of these checks —
// found by an adversarial round, not by the suite. They are listed as exact
// upload bodies because that is what they were: complete requests the door
// answered 200 to.
//
// A default-deny allowlist that admits arbitrary bytes carrying a few
// incidental ones at a fixed offset has been widened no matter what the
// allowlist contains, so these are the cases that decide whether this change
// is a fix or a hole.
func TestStructuralValidationRefusesNearMisses(t *testing.T) {
	zeros262 := make([]byte, 262)
	copy(zeros262[257:], []byte("ustar"))

	adtsBadRate := append([]byte(nil), fixture(t, "aac-adts.head512")...)
	adtsBadRate[2] |= 0x3C // sampling-frequency index 15, a reserved value

	cases := []struct {
		name string
		body []byte
		as   string
		why  string
	}{
		{"ELF carrying ustar at offset 257", fixture(t, "elf-with-ustar-magic.head512"), "p.bin",
			"a working executable was stored as application/x-tar; the header checksum is what refuses it"},
		{"zeros with ustar at offset 257", zeros262, "p.bin",
			"262 bytes of nothing is not an archive"},
		{"7z signature and nothing else", []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}, "p.bin",
			"six bytes cannot carry the start header its CRC covers"},
		{"fLaC with no STREAMINFO", []byte("fLaC\x00"), "p.bin",
			"the marker is present; the mandatory first metadata block is not"},
		{"BZh9 with no block magic", []byte("BZh9\x00"), "p.bin",
			"bzip2 streams continue with a block magic or an end-of-stream magic; there is no third case"},
		{"BZh9 long enough to reach the block magic, carrying the wrong one",
			append([]byte("BZh9\xde\xad\xbe\xef\xde\xad"), make([]byte, 32)...), "p.bin",
			"the short case above is refused by the LENGTH guard before the magic is read, so on " +
				"its own it says nothing about the magic check; the NUL padding keeps this one " +
				"binary, so the stdlib says octet-stream and the magic table is actually consulted"},
		{"three-byte ADTS", []byte{0xFF, 0xF1, 0x00}, "p.aac",
			"three bytes cannot contain a seven-byte header"},
		{"ADTS with a reserved sampling-rate index", adtsBadRate, "p.aac",
			"index 15 is reserved, so a header carrying it is not a frame"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, _, err := ValidateUpload(tc.body, tc.as)
			if err == nil {
				t.Errorf("accepted and stored as %q — %s", entry.MIME, tc.why)
			}
		})
	}
}

// TestEBMLDocTypeIsParsedNotSearched pins the DocType read in all four
// directions. The first two are the ordinary files. The last two are the pair
// an adversarial round used to break a substring search, and they broke it
// BOTH ways — the reason this is a parse now.
//
// Both adversarial fixtures are complete files that ffprobe reads, built by
// inserting a legal Void element (ID 0xEC, contents meaningless by
// specification) into the ordinary fixtures and widening the header's size
// field to match.
func TestEBMLDocTypeIsParsedNotSearched(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
		why     string
	}{
		{"matroska.head512", "video/x-matroska", "the ordinary Matroska file"},
		{"webm.head512", "video/webm", "the ordinary WebM file — a read that always said Matroska would break this"},
		{"webm-void-says-matroska.head512", "video/webm",
			"a WebM whose Void padding contains the string \"matroska\"; its real DocType is webm"},
		{"matroska-void-padded.head512", "video/x-matroska",
			"a Matroska whose Void padding pushes DocType to offset 66 — past any fixed leading window"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			if got := SniffMIME(fixture(t, tc.fixture)); got != tc.want {
				t.Errorf("SniffMIME = %q, want %q — %s", got, tc.want, tc.why)
			}
		})
	}

	// EBML magic, then bytes that parse to no DocType at all. The fallback is
	// the stdlib's answer, so an unreadable header degrades to the behaviour
	// that shipped before this check existed rather than to a refusal.
	blank := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 120)...)
	if got := SniffMIME(blank); got != "video/webm" {
		t.Errorf("EBML header with no DocType sniffed %q, want the stdlib's video/webm", got)
	}
}

// TestOggAliasIsCodecGated covers the re-ruling that followed an adversarial
// round: application/ogg is a CONTAINER name, and mapping it to audio/ogg
// unconditionally made a real VP8-in-Ogg video uploadable as audio, category
// audio, served inline — a video format the allowlist never reviewed. The
// alias is now conditional on the first packet naming an audio codec.
func TestOggAliasIsCodecGated(t *testing.T) {
	if got := SniffMIME(fixture(t, "ogg-opus.head512")); got != "audio/ogg" {
		t.Errorf("Opus-in-Ogg sniffed %q, want audio/ogg", got)
	}

	video := fixture(t, "ogg-vp8-video.head512")
	if got := SniffMIME(video); got != "application/ogg" {
		t.Errorf("VP8-in-Ogg sniffed %q, want the unaliased application/ogg", got)
	}
	if entry, _, err := ValidateUpload(video, "clip.ogv"); err == nil {
		t.Errorf("VP8-in-Ogg accepted and stored as %q; video/ogg is not on the allowlist "+
			"and admitting a format is a review, not a side effect", entry.MIME)
	}
}

// TestADTSGuardsAreBothLoadBearing exists because the previous version of
// these tests did NOT establish the guards it appeared to protect: its
// negative legs failed for reasons other than the guard under test, so both
// the octet-stream gate and the layer-bit mask could be removed with the suite
// still green. Each leg below is chosen so that exactly one mutation kills it.
func TestADTSGuardsAreBothLoadBearing(t *testing.T) {
	adts := fixture(t, "aac-adts.head512")

	if _, _, err := ValidateUpload(adts, "track.aac"); err != nil {
		t.Fatalf("premise failed: the real ADTS fixture named .aac is rejected: %v", err)
	}

	// Bytes without the extension: the structure alone must not introduce a
	// type, since nothing asked for that reading.
	if _, code, err := ValidateUpload(adts, "track.bin"); err == nil {
		t.Error("ADTS bytes named .bin were accepted; the extension gate is not applied")
	} else if code != "mime_not_allowed" {
		t.Errorf("code = %q, want mime_not_allowed", code)
	}

	// Extension without the structure: the name alone must not introduce one.
	if _, code, err := ValidateUpload(make([]byte, 512), "track.aac"); err == nil {
		t.Error("zero bytes named .aac were accepted; the extension is being trusted alone")
	} else if code != "mime_not_allowed" {
		t.Errorf("code = %q, want mime_not_allowed", code)
	}

	// The octet-stream gate, isolated. These two bytes carry a valid ADTS
	// sync AND the stdlib types them as text/plain, so the ONLY thing keeping
	// the AAC branch from firing is the gate on the stdlib's verdict. Remove
	// it and this leg flips from a category mismatch to an accepted AAC.
	if _, code, _ := ValidateUpload([]byte{0xFF, 0xF1}, "short.aac"); code != "mime_extension_mismatch" {
		t.Errorf("two ADTS sync bytes named .aac gave code %q, want mime_extension_mismatch — "+
			"the stdlib called them text, and the branch must not run on a type it recognised", code)
	}

	// The octet-stream gate, isolated at full strength. These seven bytes are
	// a STRUCTURALLY VALID ADTS header — sync, layer 00, sampling index 0,
	// frame length 2570 — and every byte is one the stdlib reads as text, so
	// it answers text/plain. That combination is the only thing that can tell
	// the gate apart from the structural check: with the gate removed, this
	// file named .aac is stored as audio/aac. The two-byte case above cannot
	// show it, because two bytes fail validADTSHeader on length first.
	textualADTS := []byte{0xFF, 0xF1, 0x40, 0x41, 0x41, 0x41, 0x41}
	if !validADTSHeader(textualADTS) {
		t.Fatal("premise failed: the input must be a valid ADTS header, " +
			"or this says nothing about the stdlib gate")
	}
	if _, code, err := ValidateUpload(textualADTS, "textual.aac"); err == nil {
		t.Error("a valid ADTS header that the stdlib reads as TEXT was accepted; " +
			"the branch must not run on a type the stdlib recognised")
	} else if code != "mime_extension_mismatch" {
		t.Errorf("code = %q, want mime_extension_mismatch", code)
	}

	// The layer-bit mask, isolated. Layer bits of 01 are invalid for ADTS and
	// are what distinguishes the 0xF6 mask from a bare 0xF0 sync check; a
	// zero-filled negative never exercises them.
	badLayer := append([]byte(nil), adts...)
	badLayer[1] = badLayer[1]&0xF9 | 0x02
	if _, _, err := ValidateUpload(badLayer, "track.aac"); err == nil {
		t.Error("ADTS with non-zero layer bits was accepted; the layer check is not load-bearing")
	}
}

// TestOpaqueMagicLosesToTheStdlib pins the ordering: the magic table is
// consulted only where the stdlib had no opinion, so it can add a detection
// and never replace one.
//
// The fixture is a REAL tar archive whose first member is named BM.txt, which
// makes the stdlib answer image/bmp. That is a genuine limitation of this
// design and is asserted rather than hidden: such a tar is refused today and
// still is. An earlier version of this test used a spliced PNG and claimed no
// real file exercised competing detections, which was false.
//
// The mutation that kills this test is hoisting sniffOpaqueMagic ABOVE the
// stdlib call. Deleting the application/octet-stream case does NOT — that is
// caught by the positive tests instead. The distinction is recorded because
// the comment here previously named the wrong mutation, contradicting the
// project's own matrix.
func TestOpaqueMagicLosesToTheStdlib(t *testing.T) {
	tarBMP := fixture(t, "tar-bmp-firstmember.head512")

	if !validTarHeader(tarBMP) {
		t.Fatal("premise failed: the fixture must be a structurally valid tar, " +
			"or this asserts nothing about which check wins")
	}
	if got := SniffMIME(tarBMP); got != "image/bmp" {
		t.Errorf("SniffMIME = %q, want image/bmp — a structurally valid tar must not "+
			"override a type the stdlib recognised", got)
	}
	if _, code, err := ValidateUpload(tarBMP, "archive.tar"); err == nil {
		t.Error("accepted; a tar the stdlib reads as an image is refused, as it was before this change")
	} else if code == "" {
		t.Errorf("rejected with an empty code")
	}
}

// TestOpaqueMagicBoundaries walks the exact lengths each check indexes past.
// The negative table below pads to 512, which hides every length guard — a
// previous version did only that, and the tar guard could be moved from 262 to
// 261 (a panic on a 261-byte input) with the suite still green.
func TestOpaqueMagicBoundaries(t *testing.T) {
	withUstar := func(n int) []byte {
		b := make([]byte, n)
		if n >= 262 {
			copy(b[257:], []byte("ustar"))
		}
		return b
	}
	for _, n := range []int{0, 1, 4, 7, 9, 31, 260, 261, 262, 511, 512} {
		// The assertion is that none of these panic and none is a false
		// positive; zero-filled input is no format.
		if got := sniffOpaqueMagic(withUstar(n)); got != "" {
			t.Errorf("sniffOpaqueMagic(%d zero bytes) = %q, want no match", n, got)
		}
	}
	if got := sniffOpaqueMagic(nil); got != "" {
		t.Errorf("sniffOpaqueMagic(nil) = %q, want no match", got)
	}
	// Truncating a REAL archive below each check's minimum must also refuse
	// rather than panic — the case a caller hits with a short upload.
	for _, f := range []string{"tar.head512", "sevenzip.head512", "flac.head512", "bzip2.head512"} {
		full := fixture(t, f)
		for n := 0; n < len(full) && n < 40; n++ {
			sniffOpaqueMagic(full[:n])
		}
	}
}

// TestOpaqueMagicNegatives pins the tightenings inside each check that the
// positive cases pass with or without.
func TestOpaqueMagicNegatives(t *testing.T) {
	pad := func(b []byte) []byte {
		out := make([]byte, 512)
		copy(out, b)
		return out
	}
	flac := func(mutate func([]byte)) []byte {
		b := pad([]byte("fLaC"))
		b[4] = 0x00
		b[5], b[6], b[7] = 0, 0, 34
		mutate(b)
		return b
	}
	cases := []struct {
		name string
		in   []byte
		why  string
	}{
		{"BZh without the block-size digit", pad([]byte("BZhX\x31\x41\x59\x26\x53\x59")),
			"the digit is part of the format, not decoration"},
		{"BZh0, an out-of-range block size", pad([]byte("BZh0\x31\x41\x59\x26\x53\x59")),
			"bzip2 block sizes are 1..9"},
		{"ustar at 256 rather than 257", func() []byte {
			b := make([]byte, 512)
			copy(b[256:], []byte("ustar"))
			return b
		}(), "the tar magic is at a fixed offset"},
		{"fLaC not at the start", pad([]byte("\x00\x00fLaC")),
			"the marker opens the stream; anywhere else it is just bytes"},
		{"truncated 7z signature", pad([]byte{0x37, 0x7A, 0xBC, 0xAF, 0x27}),
			"five of six signature bytes is not the signature"},
		{"7z signature with a wrong start-header CRC", pad([]byte{
			0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C, 0x00, 0x04,
			0xDE, 0xAD, 0xBE, 0xEF, // CRC that will not match
		}), "the CRC is the structural half of this check"},
		{"FLAC first block is not STREAMINFO", flac(func(b []byte) { b[4] = 0x01 }),
			"RFC 9639 requires STREAMINFO first"},
		{"FLAC STREAMINFO of the wrong length", flac(func(b []byte) { b[7] = 33 }),
			"STREAMINFO is exactly 34 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniffOpaqueMagic(tc.in); got != "" {
				t.Errorf("sniffOpaqueMagic = %q, want no match — %s", got, tc.why)
			}
		})
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

// TestTextFamilyStillStoresAsPlain records what this PR does NOT fix, so the
// boundary is a test rather than a sentence someone has to find. These types
// remain on the allowlist and remain unreachable; making them reachable needs
// extension trust, which is a separate change under its own ruling.
func TestTextFamilyStillStoresAsPlain(t *testing.T) {
	for _, tc := range []struct{ body, name string }{
		{"alert(1);\n", "p.js"},
		{"answer: 42\n", "p.yaml"},
		{"# heading\n", "p.md"},
	} {
		entry, _, err := ValidateUpload([]byte(tc.body), tc.name)
		if err != nil {
			t.Fatalf("%s rejected: %v", tc.name, err)
		}
		if entry.MIME != "text/plain" {
			t.Errorf("%s stored as %q, want text/plain — if this changed, the F5 "+
				"extension-trust work landed and this test should move with it", tc.name, entry.MIME)
		}
	}
}
