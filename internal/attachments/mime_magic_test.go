package attachments

import (
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

// TestMagicOnlyRecognisesWhatValidationUsedToRefuse is the WIDENING, asserted
// rather than described. Every input here was refused by the structural
// validation that stood between rounds 1 and 3, and is accepted now.
//
// The ruling this implements rests on a fact about this door rather than on a
// judgement: http.DetectContentType recognises audio/mpeg from the three bytes
// "ID3", audio/mpeg is on the allowlist, and this door already serves it
// inline. Every signature below is at least as wide as that, so recognising
// them this way is the door's existing standard.
//
// What the second half of each case asserts is the property that makes it
// tolerable — where the bytes GO. Recognising a file wrongly moves it between
// reviewed allowlist entries; it must never move it into a serving bucket the
// entry does not already permit.
func TestMagicOnlyRecognisesWhatValidationUsedToRefuse(t *testing.T) {
	cases := []struct {
		name       string
		body       []byte
		as         string
		want       string
		wantInline bool
	}{
		{"ELF carrying ustar at offset 257", readFixture(t, "elf-with-ustar-magic.head512"),
			"p.tar", "application/x-tar", false},
		{"7z signature and nothing else", []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C},
			"p.7z", "application/x-7z-compressed", false},
		{"fLaC with no STREAMINFO", []byte("fLaC\x00"),
			"p.flac", "audio/flac", true},
		{"BZh9 with nothing after it", []byte("BZh9\x00"),
			"p.bz2", "application/x-bzip2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, code, err := ValidateUpload(tc.body, tc.as)
			if err != nil {
				t.Fatalf("refused (code=%s): magic-only recognition should accept this", code)
			}
			if entry.MIME != tc.want {
				t.Errorf("stored as %q, want %q", entry.MIME, tc.want)
			}
			// The bucket is the load-bearing half. Archives download; audio
			// plays inline exactly as audio/mpeg already does.
			if got := entry.ServeInline(); got != tc.wantInline {
				t.Errorf("ServeInline() = %v, want %v — recognition may move a file "+
					"between reviewed types, never into a bucket its entry forbids",
					got, tc.wantInline)
			}
		})
	}

	// The premise the whole ruling rests on, asserted so it cannot rot: the
	// stdlib really does recognise audio/mpeg from three bytes, and this door
	// really does serve it inline.
	entry, _, err := ValidateUpload([]byte("ID3\x03\x00\x00\x00"), "p.mp3")
	if err != nil {
		t.Fatalf("premise failed: an ID3 header was refused: %v", err)
	}
	if entry.MIME != "audio/mpeg" || !entry.ServeInline() {
		t.Errorf("premise failed: ID3 gave %q inline=%v, want audio/mpeg inline=true — "+
			"magic-only recognition is justified BY this being the existing standard",
			entry.MIME, entry.ServeInline())
	}
}

// TestRealFilesValidationUsedToRefuse is the direction that made the ruling
// necessary. Each is a file a user legitimately has, refused by the structural
// validation and accepted now. Refusing these is the defect BUG-2963 exists to
// fix, reintroduced by its own fix.
func TestRealFilesValidationUsedToRefuse(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		as      string
		want    string
		why     string
	}{
		{"tar-pax.head512", "archive.tar", "application/x-tar",
			"a PAX archive leads with a metadata header, so a parse needs more blocks than this door reads"},
		{"tar-gnu-longname.head512", "archive.tar", "application/x-tar",
			"a GNU archive whose first filename exceeds 100 bytes leads with a long-name header, likewise"},
		{"flac-zero-sample-rate.head512", "track.flac", "audio/flac",
			"RFC 9639 permits a zero sample rate for non-audio samples and still registers it as audio/flac"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			entry, code, err := ValidateUpload(readFixture(t, tc.fixture), tc.as)
			if err != nil {
				t.Fatalf("refused (code=%s) — %s", code, tc.why)
			}
			if entry.MIME != tc.want {
				t.Errorf("stored as %q, want %q", entry.MIME, tc.want)
			}
		})
	}
}

// TestMagicStillHasToBeThere keeps the recognisers honest in the only way that
// still applies: the magic must be present, in the right place, in full.
func TestMagicStillHasToBeThere(t *testing.T) {
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
		{"BZh without the block-size digit", pad([]byte("BZhX")), "the digit is part of the signature"},
		{"BZh0, an out-of-range block size", pad([]byte("BZh0")), "bzip2 block sizes are 1..9"},
		{"ustar at 256 rather than 257", func() []byte {
			b := make([]byte, 512)
			copy(b[256:], []byte("ustar"))
			return b
		}(), "the tar magic is at a fixed offset"},
		{"fLaC not at the start", pad([]byte("\x00\x00fLaC")), "the marker opens the stream"},
		{"five of the six 7z signature bytes", pad([]byte{0x37, 0x7A, 0xBC, 0xAF, 0x27}),
			"a truncated signature is not the signature"},
		{"a tar too short to hold the magic offset", make([]byte, 261),
			"261 bytes cannot contain a magic that ends at 262"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniffOpaqueMagic(tc.in); got != "" {
				t.Errorf("sniffOpaqueMagic = %q, want no match — %s", got, tc.why)
			}
		})
	}

	// Lengths each recogniser indexes past, walked so a bounds error shows up
	// as a failure rather than as a panic in production.
	for _, n := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 260, 261, 262, 511, 512} {
		sniffOpaqueMagic(make([]byte, n))
	}
	if got := sniffOpaqueMagic(nil); got != "" {
		t.Errorf("sniffOpaqueMagic(nil) = %q, want no match", got)
	}
}

// TestEBMLDocTypeSurvivesPaddingPastTheHead covers a round-3 finding that the
// magic-only ruling does not touch, because the DocType parse stays: a header
// declaring a DocType payload longer than the bytes we hold is still
// answerable when the VALUE is complete in hand, since it ends at its first
// NUL.
func TestEBMLDocTypeSurvivesPaddingPastTheHead(t *testing.T) {
	// EBML header declaring a 600-byte DocType payload: "matroska", a NUL,
	// and padding that runs past anything this door reads.
	body := append([]byte("matroska\x00"), make([]byte, 591)...)
	head := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x42, 0x5C, 0x42, 0x82, 0x42, 0x58}
	head = append(head, body...)
	if len(head) > 512 {
		head = head[:512]
	}
	if got := sniffEBMLDocType(head); got != "video/x-matroska" {
		t.Errorf("sniffEBMLDocType = %q, want video/x-matroska — the value is complete "+
			"in the bytes we hold even though its declared padding is not", got)
	}
}

// TestEBMLParseDetails covers the two DocType-walk properties that survive the
// magic-only ruling untouched. They were lost when the structural-validation
// tests were cut wholesale, and a mutation run caught the gap: both mutations
// below had gone from detected to surviving.
func TestEBMLParseDetails(t *testing.T) {
	t.Run("DocType ends at its first NUL", func(t *testing.T) {
		// A real Matroska whose DocType payload is "matroska\x00junk" with a
		// declared length of 13. Bytes after the terminator are padding;
		// trimming instead of terminating stored this as WebM.
		if got := SniffMIME(readFixture(t, "matroska-nul-terminated-doctype.head512")); got != "video/x-matroska" {
			t.Errorf("SniffMIME = %q, want video/x-matroska", got)
		}
	})

	t.Run("reserved all-ones EBML IDs are refused", func(t *testing.T) {
		// 0xFF is a reserved ID, not a valid element. Accepting it let junk
		// act as a zero-length child and carry the walk onward to a DocType
		// that followed it.
		bad := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x8D, 0xFF, 0x80,
			0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'}
		if got := sniffEBMLDocType(bad); got != "" {
			t.Errorf("sniffEBMLDocType = %q, want no answer — the walk crossed a reserved ID", got)
		}
		// Control: the same shape WITHOUT the reserved ID must parse, so the
		// leg above fails on the ID rather than on anything else about it.
		ok := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x8B,
			0x42, 0x82, 0x88, 'm', 'a', 't', 'r', 'o', 's', 'k', 'a'}
		if got := sniffEBMLDocType(ok); got != "video/x-matroska" {
			t.Errorf("premise failed: control sniffed %q, want video/x-matroska", got)
		}
	})
}
