package attachments

import "testing"

// FuzzSniffMIME exercises the whole sniff path — the stdlib call, both
// refinements, and the magic recognisers — against arbitrary bytes.
//
// It exists because BUG-2963 added a PARSER. Every other check in this package
// reads fixed offsets and is bounded by construction; sniffEBMLDocType walks
// caller-supplied length fields, which is the one shape here that can index
// out of range or fail to advance. The properties asserted are the two a
// sniffer owes its caller: it returns, and it does not panic. What it returns
// for nonsense is not asserted — that is the table tests' job.
//
// Seeds are the real fixtures plus the shapes a walk is most likely to break
// on: truncation at every element boundary, declared sizes larger than the
// data, and the reserved all-ones VINT that means "unknown size".
func FuzzSniffMIME(f *testing.F) {
	for _, name := range []string{
		"matroska.head512", "webm.head512", "webm-void-says-matroska.head512",
		"matroska-void-padded.head512", "matroska-void-beyond-window.head512",
		"tar.head512", "tar-bmp-firstmember.head512",
		"sevenzip.head512", "flac.head512", "bzip2.head512", "aac-adts.head512",
		"ogg-opus.head512", "ogg-vp8-video.head512", "avi.head512",
		"elf-with-ustar-magic.head512",
	} {
		b, err := fixtureBytes(name)
		if err != nil {
			f.Fatalf("seed %s: %v", name, err)
		}
		f.Add(b)
		// Truncations: the walk must survive running out mid-element.
		for _, n := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 24, 40, 64, 100} {
			if n < len(b) {
				f.Add(b[:n])
			}
		}
	}
	// EBML magic followed by hostile length fields.
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0xFF})                                       // unknown-size header
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0xFE, 0x42, 0x82, 0xFF})                     // unknown-size child
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0xA3, 0x42, 0x82, 0x88, 'm', 'a', 't', 'r'}) // child longer than the data
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0xA3, 0x00, 0x00})                           // invalid all-zero VINT marker
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3, 0xA3, 0x80, 0x80})                           // zero-length child, must still advance
	f.Add([]byte("OggS\x00\x02"))                                                     // truncated Ogg page header

	f.Fuzz(func(t *testing.T, head []byte) {
		// Each is called directly as well as through SniffMIME, so a panic is
		// attributed to the check that owns it rather than to the dispatcher.
		sniffOpaqueMagic(head)
		sniffEBMLDocType(head)
		validADTSHeader(head)
		SniffMIME(head)
		ValidateUpload(head, "fuzz.aac")
		ValidateUpload(head, "fuzz.mkv")
		ValidateUpload(head, "fuzz.tar")
	})
}
