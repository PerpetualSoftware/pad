package attachments

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// fixtureBytes is the testing-free half of this package's fixture loading, so
// a *testing.F can build a fuzz seed corpus from the same files that
// readFixture (mime_isobmff_test.go) hands the table tests.
func fixtureBytes(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", name))
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
		{"matroska.head512", "clip.mkv", "video/x-matroska", "", CategoryVideo, "EBML DocType matroska"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			head := readFixture(t, tc.fixture)
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

	adtsBadRate := append([]byte(nil), readFixture(t, "aac-adts.head512")...)
	adtsBadRate[2] |= 0x3C // sampling-frequency index 15, a reserved value

	cases := []struct {
		name string
		body []byte
		as   string
		why  string
	}{
		{"ELF carrying ustar at offset 257", readFixture(t, "elf-with-ustar-magic.head512"), "p.bin",
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
// Both adversarial fixtures were built by inserting a legal Void element (ID
// 0xEC, contents meaningless by specification) into the ordinary fixtures and
// widening the header's size field to match. The COMPLETE files ffprobe reads;
// what is committed here is their first 512 bytes, like every fixture in this
// package, because that is the whole input domain of SniffMIME. Running
// ffprobe on the committed prefix fails with a premature EOF, which says
// nothing about the file it came from — an earlier version of this comment
// claimed the committed files were themselves readable, and they are not.
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
			if got := SniffMIME(readFixture(t, tc.fixture)); got != tc.want {
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

// TestOggStaysRefused records a decision, not a mechanism: Ogg is NOT
// recognised, and both fixtures are kept so the next person to reach for an
// application/ogg alias meets the evidence first.
//
// An alias was written, ruled in, and then removed after review showed the
// question it has to answer — is this container AUDIO — cannot be answered
// from the head of the file. Ogg multiplexes, so a second video stream's pages
// come later than any sniff can see. Both files below are refused today
// exactly as they were before BUG-2963, which is the point: no regression, and
// no acceptance of a type the allowlist never reviewed.
func TestOggStaysRefused(t *testing.T) {
	for _, f := range []string{"ogg-opus.head512", "ogg-vp8-video.head512"} {
		t.Run(f, func(t *testing.T) {
			if got := SniffMIME(readFixture(t, f)); got != "application/ogg" {
				t.Errorf("SniffMIME = %q, want the unaliased application/ogg", got)
			}
			if entry, _, err := ValidateUpload(readFixture(t, f), "track.ogg"); err == nil {
				t.Errorf("accepted and stored as %q; audio/ogg stays unreachable until "+
					"either video/ogg is a reviewed allowlist entry or something "+
					"demuxes far enough to enumerate the streams", entry.MIME)
			}
		})
	}
}

// TestADTSGuardsAreBothLoadBearing exists because the previous version of
// these tests did NOT establish the guards it appeared to protect: its
// negative legs failed for reasons other than the guard under test, so both
// the octet-stream gate and the layer-bit mask could be removed with the suite
// still green. Each leg below is chosen so that exactly one mutation kills it.
func TestADTSGuardsAreBothLoadBearing(t *testing.T) {
	adts := readFixture(t, "aac-adts.head512")

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

	// Two ADTS sync bytes named .aac. This leg does NOT isolate the
	// octet-stream gate — validADTSHeader refuses two bytes on length before
	// the gate could matter, so removing the gate leaves this passing. It is
	// kept as an ordinary category-mismatch case, and the leg that actually
	// isolates the gate is the seven-byte one below. The comment here claimed
	// the wrong mutation twice; it now says what is true.
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
	tarBMP := readFixture(t, "tar-bmp-firstmember.head512")

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
	// Truncating a REAL archive below its check's minimum must REFUSE, not
	// merely fail to panic. An earlier version of this loop discarded the
	// return value and stopped at 40 bytes, so it asserted nothing at all —
	// flac.head512[:8] was accepted while the test passed.
	//
	// The bound per format is the smallest prefix its check can accept; below
	// it the answer must be "". At and above it, a real archive's prefix is
	// legitimately recognised, so nothing is asserted there.
	for _, tc := range []struct {
		fixture string
		min     int
	}{
		{"tar.head512", 512},
		{"sevenzip.head512", 32},
		{"flac.head512", 42},
		{"bzip2.head512", 10},
	} {
		full := readFixture(t, tc.fixture)
		for n := 0; n < tc.min && n <= len(full); n++ {
			if got := sniffOpaqueMagic(full[:n]); got != "" {
				t.Errorf("sniffOpaqueMagic(%s[:%d]) = %q, want no match below the %d-byte minimum",
					tc.fixture, n, got, tc.min)
			}
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

// TestLegalVariantsAreNotRefused is the other direction, and it was missing:
// every negative case above pushes toward refusing more, and nothing pushed
// back. A review round confirmed the gap by removing bzip2's end-of-stream
// acceptance, FLAC's last-block mask and the ADTS protection arithmetic with
// the suite still green.
func TestLegalVariantsAreNotRefused(t *testing.T) {
	// An EMPTY bzip2 stream: the end-of-stream magic comes first, with the
	// combined CRC of nothing. Produced by `bzip2 -c < /dev/null`.
	emptyBz2 := []byte{
		0x42, 0x5A, 0x68, 0x39, // BZh9
		0x17, 0x72, 0x45, 0x38, 0x50, 0x90, // end-of-stream magic
		0x00, 0x00, 0x00, 0x00, // combined CRC of an empty stream
	}
	if !validBzip2Stream(emptyBz2) {
		t.Error("an empty bzip2 stream was refused; the end-of-stream magic is a legal first block")
	}

	// STREAMINFO carrying the LAST-BLOCK flag: legal, and the only shape a
	// FLAC file with no other metadata blocks can have.
	flac := append([]byte(nil), readFixture(t, "flac.head512")...)
	flac[4] |= 0x80
	if !validFLACStream(flac) {
		t.Error("STREAMINFO marked as the last metadata block was refused; the flag is legal " +
			"and the type check must mask it off")
	}

	// A CRC-PROTECTED ADTS frame: protection_absent clear, frame length long
	// enough for the nine-byte protected header.
	adts := append([]byte(nil), readFixture(t, "aac-adts.head512")...)
	adts[1] &^= 0x01 // protection present
	if !validADTSHeader(adts) {
		t.Error("a CRC-protected ADTS frame was refused; protection is optional, not invalid")
	}

	// ...and the arithmetic that check turns on: the same frame declaring a
	// length that fits the unprotected header but not the protected one.
	tooShort := append([]byte(nil), adts...)
	tooShort[3] = tooShort[3]&^0x03 | 0x00
	tooShort[4] = 0x01 // frame length 8: >= 7, < 9
	tooShort[5] &^= 0xE0
	if validADTSHeader(tooShort) {
		t.Error("a protected ADTS frame declaring 8 bytes was accepted; its own header needs 9")
	}
}

// TestTarAndELFAreNotDistinguishableHere records a LIMITATION as a test,
// because it is the kind that otherwise gets rediscovered as a bug.
//
// The fixture is an ELF header carrying a well-formed tar header in its
// padding — `ustar` at 257, valid octal mode/uid/gid/size/mtime, and a
// correctly computed checksum. It is ACCEPTED as application/x-tar, and it is
// accepted by archive/tar's own Reader.Next, which is the parser this package
// delegates to. A 512-byte tar header is exactly those fields; nothing forbids
// another format's padding from containing them.
//
// So this asserts the current, understood behaviour rather than a wish. If it
// ever starts failing, someone has found a discriminator that the Go standard
// library does not have — which is interesting and should be read, not
// silently accommodated.
func TestTarAndELFAreNotDistinguishableHere(t *testing.T) {
	b := readFixture(t, "elf-with-valid-tar-checksum.head512")
	if string(b[1:4]) != "ELF" {
		t.Fatal("premise failed: the fixture must still be an ELF header")
	}
	entry, _, err := ValidateUpload(b, "p.bin")
	if err != nil {
		t.Fatalf("refused (%v) — if this is a deliberate improvement, replace this test "+
			"and say what discriminates the two", err)
	}
	if entry.MIME != "application/x-tar" {
		t.Errorf("stored as %q, want application/x-tar", entry.MIME)
	}
	// The property that makes the above tolerable is where the bytes GO, not
	// what they are called: an archive is never served inline.
	if entry.ServeInline() {
		t.Error("application/x-tar is inline-safe; it must be served as an attachment")
	}
}

// TestRoundTwoSurvivors covers the guards a rebuilt mutation matrix found
// nothing testing. Each case below is the exact input a review round used, or
// the smallest one that separates the guard from everything around it.
func TestRoundTwoSurvivors(t *testing.T) {
	t.Run("7z start-header arithmetic must be representable", func(t *testing.T) {
		// A CRC proves the twenty bytes are the intended ones. These are
		// intended and impossible: a next-header offset of 2^64-1 cannot have
		// the 32-byte signature header added to it.
		b := make([]byte, 64)
		copy(b, []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C, 0x00, 0x04})
		for i := 12; i < 20; i++ {
			b[i] = 0xFF // NextHeaderOffset = max uint64
		}
		b[20] = 0x01 // NextHeaderSize = 1
		binary.LittleEndian.PutUint32(b[8:12], crc32.ChecksumIEEE(b[12:32]))
		if validSevenZipHeader(b) {
			t.Error("accepted a start header whose next-header position cannot be represented")
		}
		// Control: the same header with a sane offset, so the leg above is
		// failing on the arithmetic and not on the CRC.
		for i := 12; i < 20; i++ {
			b[i] = 0
		}
		binary.LittleEndian.PutUint32(b[8:12], crc32.ChecksumIEEE(b[12:32]))
		if !validSevenZipHeader(b) {
			t.Error("premise failed: the control header must be accepted, " +
				"or the case above proves nothing about the arithmetic")
		}
	})

	t.Run("FLAC sample rate may not be zero", func(t *testing.T) {
		b := append([]byte(nil), readFixture(t, "flac.head512")...)
		// Sample rate is the 20 bits starting at byte 18 of the file
		// (STREAMINFO byte 10). Zero is reserved for a non-audio stream.
		b[18], b[19] = 0, 0
		b[20] &^= 0xF0
		if validFLACStream(b) {
			t.Error("accepted STREAMINFO declaring a zero sample rate")
		}
	})

	t.Run("a truncated real bzip2 stream is still recognised", func(t *testing.T) {
		// The head of a 200KB archive: the decoder cannot finish, and must
		// not be allowed to refuse on that account. This is the case that
		// distinguishes "structural error" from "ran out of bytes".
		if !validBzip2Stream(readFixture(t, "bzip2-truncated.head512")) {
			t.Error("a truncated bzip2 stream was refused; truncation is what a 512-byte " +
				"head of any real archive looks like")
		}
	})

	t.Run("DocType ends at its first NUL", func(t *testing.T) {
		// A real Matroska whose DocType payload is "matroska\x00junk" with a
		// declared length of 13. Trailing bytes after the terminator are not
		// part of the value; trimming instead of terminating stored this as
		// WebM.
		if got := SniffMIME(readFixture(t, "matroska-nul-terminated-doctype.head512")); got != "video/x-matroska" {
			t.Errorf("SniffMIME = %q, want video/x-matroska", got)
		}
	})

	t.Run("reserved all-ones EBML IDs are refused", func(t *testing.T) {
		// 0xFF is a reserved ID, not a valid element. Accepting it let junk
		// act as a zero-length child and carry the walk onward to a DocType
		// that followed it.
		b := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x8D, 0xFF, 0x80,
			0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'}
		if got := sniffEBMLDocType(b); got != "" {
			t.Errorf("sniffEBMLDocType = %q, want no answer — the walk crossed a reserved ID", got)
		}
		// Control: the same bytes without the reserved ID must parse, so the
		// leg above fails on the ID and not on the rest of the shape.
		ok := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x8B,
			0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'}
		if got := sniffEBMLDocType(ok); got != "video/x-matroska" {
			t.Errorf("premise failed: control sniffed %q, want video/x-matroska", got)
		}
	})
}

// TestRoundTwoSurvivorsPartTwo covers three more guards a repaired mutation
// run found untested — the first two only reachable once the mutants that had
// been failing to COMPILE were rewritten to compile, which is why they hid: a
// build failure scores as nothing, not as a survivor.
func TestRoundTwoSurvivorsPartTwo(t *testing.T) {
	streamInfo := func(mutate func([]byte)) []byte {
		b := append([]byte(nil), readFixture(t, "flac.head512")...)
		mutate(b)
		return b
	}

	t.Run("FLAC minimum block size below the format's floor", func(t *testing.T) {
		// STREAMINFO bytes 0..1 are the minimum block size; the format sets a
		// floor of 16 samples.
		if validFLACStream(streamInfo(func(b []byte) { b[8], b[9] = 0, 4 })) {
			t.Error("accepted a minimum block size of 4")
		}
	})

	t.Run("FLAC block sizes out of order", func(t *testing.T) {
		// Maximum below minimum is not a stream any encoder can produce.
		if validFLACStream(streamInfo(func(b []byte) {
			b[8], b[9] = 0x10, 0x00   // min 4096
			b[10], b[11] = 0x00, 0x20 // max 32
		})) {
			t.Error("accepted a maximum block size below the minimum")
		}
	})

	t.Run("bzip2 empty stream with an invalid combined CRC", func(t *testing.T) {
		// The case only the DECODE can catch: a well-formed empty stream whose
		// checksum is wrong. Nothing in the header is out of place.
		bad := []byte{0x42, 0x5A, 0x68, 0x39, 0x17, 0x72, 0x45, 0x38, 0x50, 0x90,
			0xDE, 0xAD, 0xBE, 0xEF}
		if validBzip2Stream(bad) {
			t.Error("accepted an empty bzip2 stream carrying an invalid combined CRC; " +
				"only decoding reads that checksum")
		}
	})
}
