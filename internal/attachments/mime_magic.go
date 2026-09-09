package attachments

import (
	"bytes"
	"strings"
)

// This file closes the second half of the gap BUG-2961 opened: the upload
// allowlist names types that SniffMIME cannot produce, so the door refuses —
// or silently retypes — files the allowlist says are supported. A measurement
// over real files (BUG-2963) found 25 of 48 allowlist entries unreachable as a
// stored type.
//
// WHAT THESE CHECKS DO. They RECOGNISE a format from its defining magic, to
// the same standard the standard library's own detector uses. They do not
// prove the bytes are that format, and three review rounds established that
// nothing available here could:
//
//   - Round 1 defeated magic-only matching with a real, executing ELF binary
//     carrying "ustar" at offset 257, stored as application/x-tar.
//   - Round 2 defeated the checksum that was round 1's answer, with an ELF
//     carrying a CORRECT one — and archive/tar's own Reader.Next accepts that
//     file too. A 512-byte tar header is exactly those fields, so the two are
//     not distinguishable at this size by anything in the standard library.
//   - Round 3 found the accumulated validation refusing REAL files: PAX and
//     long-name GNU tars, legal randomized bzip2 blocks, FLAC declaring the
//     zero sample rate RFC 9639 permits. That is this bug's own defect —
//     refusing files people legitimately have — reintroduced by the fix.
//
// So the structural validators are gone and recognition is by magic. The
// settling fact is that this is the door's EXISTING standard, not a relaxation
// of it: http.DetectContentType recognises audio/mpeg from the three bytes
// "ID3" (net/http/sniff.go), audio/mpeg is on the allowlist, and this door
// already serves it inline. Every signature here is at least as wide.
//
// THE SAFETY PROPERTY, written from what the code does — two earlier versions
// of this paragraph were checked and found factually wrong:
//
//   - Nothing here is executed, and nothing is decompressed. An earlier
//     version decoded bzip2 while sniffing; that is gone.
//   - Serving is per allowlist entry, not uniform. Archives are RenderChip and
//     absent from inlineSafe, so they download. audio/flac and audio/aac are
//     inlineSafe like every other allowlisted audio type and play through an
//     <audio> element, which renders without executing embedded script.
//   - nosniff is set, so a browser will not re-interpret the bytes as
//     something more dangerous than the type stored, and that type is on a
//     reviewed allowlist. Recognising a format wrongly moves a file between
//     reviewed types; it cannot move it outside them.
//
// No filename is trusted to introduce a type here. The one place an extension
// participates is documented at validADTSHeader, and it only DISAMBIGUATES a
// structure the bytes must already carry.

// sniffOpaqueMagic recognises formats the WHATWG mimesniff table has no
// signature for, so http.DetectContentType answers application/octet-stream
// for a real file of them. It returns the DEFAULT candidate; see
// sniffOpaqueCandidates for why there can be more than one.
//
// Called ONLY when the stdlib returned application/octet-stream, so it cannot
// override a type the standard library identified.
func sniffOpaqueMagic(head []byte) string {
	if c := sniffOpaqueCandidates(head); len(c) > 0 {
		return c[0]
	}
	return ""
}

// sniffOpaqueCandidates returns EVERY type whose magic matches, because more
// than one can, and which is right is not always decidable from the bytes.
//
// The collision is real in both directions and neither side is exotic:
//
//   - A tar header's first 100 bytes are its member's FILENAME, so an ordinary
//     archive whose first member is called "fLaC.txt" carries the FLAC marker
//     at offset zero.
//   - A FLAC file's Vorbis COMMENT tags are arbitrary UTF-8 (RFC 9639 §8.6),
//     and an AAC frame's ancillary payload is arbitrary bytes, so either can
//     contain "ustar" at offset 257.
//
// An earlier version picked a winner by ordering and argued the collision was
// asymmetric — that real audio could not plausibly carry "ustar" at a fixed
// offset. Review refuted that with complete, decodable FLAC and AAC files
// carrying it in ordinary metadata. Any total order refuses somebody: tar
// first refuses those, prefixes first refuse the archive.
//
// So the order here is only a DEFAULT. ValidateUpload, which knows the
// filename, may pick a different candidate from this list — see
// preferCandidateForExt, and note what that is and is not: the extension
// cannot introduce a type, only choose among readings the bytes themselves
// support.
func sniffOpaqueCandidates(head []byte) []string {
	var out []string
	// tar leads the default order because its magic sits at a fixed offset
	// rather than at a prefix, so it is the one least likely to be an
	// accident of some other format's leading bytes.
	if validTarHeader(head) {
		out = append(out, "application/x-tar")
	}
	if validSevenZipHeader(head) {
		out = append(out, "application/x-7z-compressed")
	}
	if validFLACStream(head) {
		out = append(out, "audio/flac")
	}
	if validBzip2Stream(head) {
		out = append(out, "application/x-bzip2")
	}
	return out
}

// preferCandidateForExt returns the candidate that the filename's extension
// names, or "" when the extension names none of them.
//
// This is the ONLY place a filename influences which type is chosen, and the
// influence is deliberately weak: every candidate is a type the BYTES already
// matched, so the extension breaks a tie rather than casting a vote. A name
// that matches nothing in the list changes nothing, and a name for a type
// whose magic is absent can never appear in the list at all.
func preferCandidateForExt(candidates []string, ext string) string {
	if len(candidates) < 2 || ext == "" {
		return ""
	}
	want, ok := extMIMEMap[strings.ToLower(ext)]
	if !ok {
		return ""
	}
	want = NormalizeMIME(want)
	for _, c := range candidates {
		if c == want {
			return c
		}
	}
	return ""
}

// validSevenZipHeader recognises a 7z archive by its six-byte signature.
//
// There was a start-header CRC check here. It is gone with the rest of the
// structural validation (see this file's header): it could not narrow what the
// door accepts, and its neighbours had begun refusing real archives.
func validSevenZipHeader(head []byte) bool {
	return bytes.HasPrefix(head, []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C})
}

// validFLACStream recognises a native FLAC stream by its four-byte "fLaC"
// marker (RFC 9639 §8.1).
//
// Four bytes is a wider signature than the stdlib uses for the audio type that
// is already on this allowlist and already served inline: audio/mpeg is
// recognised from the three bytes "ID3". Recognising FLAC the same way is this
// door's existing standard, not a departure from it.
func validFLACStream(head []byte) bool {
	return bytes.HasPrefix(head, []byte("fLaC"))
}

// validBzip2Stream recognises a bzip2 stream by "BZh" and its block-size digit.
//
// It does NOT decompress. An earlier version decoded through the standard
// library to reach the stream CRC, which refused legal randomized blocks Go's
// decoder does not implement — a real .bz2 rejected for a decoder's missing
// feature. Nothing in this door expands an upload now.
func validBzip2Stream(head []byte) bool {
	return len(head) >= 4 && bytes.HasPrefix(head, []byte("BZh")) &&
		head[3] >= '1' && head[3] <= '9'
}

// validTarHeader recognises a POSIX ustar archive by the magic at offset 257.
//
// TWO KINDS OF FILE ARE DELIBERATELY NOT RECOGNISED, and both are refusals
// rather than oversights:
//
//   - V7 tar, which predates the magic and has no signature anywhere. There is
//     nothing at a fixed offset to key on, so it cannot be recognised by this
//     kind of check at all; it stays refused, as it was before BUG-2963.
//   - Nothing else. PAX and long-name GNU archives lead with a metadata block
//     rather than a file header, and they ARE recognised, because the magic is
//     in that block too. A full PARSE would need more blocks than this door
//     ever reads, which is one of the reasons the parse is gone.
//
// The checksum that used to be verified here is gone. It never distinguished a
// crafted header from a real one — archive/tar accepts an ELF carrying a
// correct one, which ships as a fixture — and verifying it required a full
// 512-byte block, which is why PAX and long-name GNU archives were refused.
func validTarHeader(head []byte) bool {
	const magicEnd = 262
	return len(head) >= magicEnd && bytes.Equal(head[257:magicEnd], []byte("ustar"))
}

// sniffEBMLDocType returns the MIME for an EBML file by READING ITS DOCTYPE
// ELEMENT, not by searching for a string.
//
// The mimesniff table maps the bare EBML magic (1A 45 DF A3) to video/webm
// with no DocType check, so a Matroska file — on the allowlist under its own
// name — is accepted and stored as WebM. That is not a refusal, which is why
// it survived a pass looking for 415s: the upload succeeds and the stored type
// is simply false.
//
// This walks the EBML header's child elements. The first version searched the
// leading 64 bytes for the two strings instead, and an adversarial round broke
// it in BOTH directions with files that ffprobe accepts: a WebM carrying
// "matroska" inside a Void element was stored as Matroska, and a Matroska with
// forty bytes of legal Void padding — which pushes DocType past 64 — was
// stored as WebM, leaving the very defect this fixes live for any file a muxer
// chose to pad. Void is legal anywhere and its contents are meaningless, so
// only a parse can tell payload from padding.
//
// Called only when the stdlib returned video/webm, so it can move a file
// between the two EBML types and nowhere else. An unreadable or absent DocType
// returns "" and the stdlib verdict stands, which is the behaviour that
// shipped before this existed.
func sniffEBMLDocType(head []byte) string {
	const ebmlHeaderID = 0x1A45DFA3
	const docTypeID = 0x4282

	id, rest, ok := readEBMLID(head)
	if !ok || id != ebmlHeaderID {
		return ""
	}
	size, rest, ok := readEBMLSize(rest)
	if !ok {
		return ""
	}
	// The header may be longer than the bytes we were given; walking what we
	// have is correct, and running out simply means no DocType was found.
	if size < uint64(len(rest)) {
		rest = rest[:size]
	}

	for len(rest) > 0 {
		childID, after, ok := readEBMLID(rest)
		if !ok {
			return ""
		}
		childSize, after, ok := readEBMLSize(after)
		if !ok {
			return ""
		}
		if childSize > uint64(len(after)) {
			// The declared payload runs past what we hold. For DocType that
			// is still answerable when the VALUE is complete in hand: it ends
			// at its first NUL, and a header padding DocType out to 600 bytes
			// puts "matroska\x00" in the first dozen. Anything else, we stop.
			if childID == docTypeID {
				if mime := docTypeMIME(after); mime != "" {
					return mime
				}
			}
			return ""
		}
		if childID == docTypeID {
			// DocType is an ASCII string whose value ENDS at the first NUL;
			// anything after that is padding and is not part of the value
			// (RFC 8794 §13). TrimRight was wrong here — a round-2 finding
			// produced an ffprobe-readable Matroska whose DocType payload was
			// "matroska\x00junk", which trimming left intact and so failed to
			// match, storing a real Matroska as WebM.
			return docTypeMIME(after[:childSize])
		}
		rest = after[childSize:]
	}
	return ""
}

// readEBMLID reads an EBML element ID. IDs keep their length marker as part of
// the value (RFC 8794 §5), so DocType is the four-hex-digit 0x4282 that the
// specification names, and the marker byte tells us how many bytes to take.
func readEBMLID(b []byte) (id uint32, rest []byte, ok bool) {
	if len(b) == 0 {
		return 0, nil, false
	}
	n := ebmlLength(b[0])
	if n == 0 || n > 4 || len(b) < n {
		return 0, nil, false
	}
	for i := 0; i < n; i++ {
		id = id<<8 | uint32(b[i])
	}
	// An ID whose value bits are all ones is RESERVED and not a valid element
	// ID (RFC 8794 §5). Accepting one let malformed bytes act as a
	// zero-length child and carry the walk onward to a DocType that follows
	// them, which a round-2 finding used to have 1a45dfa3 8d ff80 ... parse
	// as Matroska.
	if id == allOnesEBMLID[n] {
		return 0, nil, false
	}
	return id, b[n:], true
}

// allOnesEBMLID is the reserved value at each ID width: the marker bit plus
// every value bit set.
var allOnesEBMLID = [5]uint32{0, 0xFF, 0x7FFF, 0x3FFFFF, 0x1FFFFFFF}

// readEBMLSize reads an EBML data size, whose length marker is REMOVED from
// the value (unlike an ID). An all-ones value means "unknown size", which this
// treats as unreadable — a header of unknown length is not something to walk.
func readEBMLSize(b []byte) (size uint64, rest []byte, ok bool) {
	if len(b) == 0 {
		return 0, nil, false
	}
	n := ebmlLength(b[0])
	if n == 0 || n > 8 || len(b) < n {
		return 0, nil, false
	}
	size = uint64(b[0]) & (1<<(8-uint(n)) - 1)
	allOnes := size == 1<<(8-uint(n))-1
	for i := 1; i < n; i++ {
		size = size<<8 | uint64(b[i])
		allOnes = allOnes && b[i] == 0xFF
	}
	if allOnes {
		return 0, nil, false
	}
	return size, b[n:], true
}

// ebmlLength returns how many bytes a VINT starting with this byte occupies —
// one plus the number of leading zero bits — or 0 for the invalid all-zero
// marker byte.
func ebmlLength(first byte) int {
	for i := 0; i < 8; i++ {
		if first&(0x80>>uint(i)) != 0 {
			return i + 1
		}
	}
	return 0
}

// validADTSHeader recognises the ADTS framing raw .aac files use: the 12-bit
// syncword plus the two layer bits ADTS requires to be zero.
//
// THIS SIGNATURE IS WEAKER THAN THE REST, and that is why its caller treats it
// differently. Fourteen bits at offset zero is less than the three bytes the
// stdlib uses for audio/mpeg, and MPEG audio shares the syncword's shape. So
// ValidateUpload also requires the .aac EXTENSION — not as evidence, but to
// decide whether a signature this weak may speak at all.
//
// The frame-length, sampling-index and protection arithmetic that used to be
// here is gone with the other structural validation. Review found it wrong in
// both directions — accepting frames whose declared length could not hold
// their own header, and rejecting legal MPEG-2 profiles — which is the tail
// this file's header describes.
//
// Layout (ISO/IEC 13818-7 §6.2, adts_fixed_header):
//
//	byte 0   syncword high 8 bits
//	byte 1   sync low 4 | MPEG version | layer (2 bits) | protection absent
func validADTSHeader(head []byte) bool {
	if len(head) < 2 {
		return false
	}
	if head[0] != 0xFF || head[1]&0xF0 != 0xF0 {
		return false
	}
	return head[1]>>1&0x03 == 0 // layer MUST be 00 for ADTS
}

// docTypeMIME maps a DocType payload to a MIME type. The value ENDS at its
// first NUL and anything after is padding (RFC 8794 §13); a payload with no
// NUL at all is the whole slice. A DocType this does not recognise returns ""
// — not ours to name, and the stdlib's answer is no worse than a guess.
func docTypeMIME(payload []byte) string {
	if i := bytes.IndexByte(payload, 0); i >= 0 {
		payload = payload[:i]
	}
	switch string(payload) {
	case "matroska":
		return "video/x-matroska"
	case "webm":
		return "video/webm"
	}
	return ""
}

// validCFBHeader recognises a Compound File Binary container by its eight-byte
// signature (MS-CFB §2.2). It is the container legacy Office wrote .doc, .xls
// and .ppt into, and the stdlib has no signature for it, so every one of those
// files sniffed application/octet-stream and was refused while all three types
// sat on the allowlist.
//
// EIGHT FIXED BYTES AND NOTHING ELSE, deliberately. CFB is a container, not a
// format: Word, Excel, PowerPoint, Visio, and .msi installers all wear this
// header, and telling them apart means walking the directory stream, which is
// a parse this door does not do (see this file's header for the three rounds
// that settled why). So this says only "this is a CFB container" and the
// caller decides what may be believed on top of it — which is why the caller
// requires an extension naming one of the three reviewed Office types rather
// than trusting the header alone. A .msi renamed .doc is stored as a Word
// document, downloads like one, and executes no more than any other refused
// byte string would.
func validCFBHeader(head []byte) bool {
	return bytes.HasPrefix(head, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
}

// validRTFStream recognises Rich Text Format by its opening "{\rtf1" (the RTF
// specification's required version-1 header).
//
// Unlike the CFB header above this needs no extension: five bytes at offset
// zero, one of which is a digit fixed by the spec, is a stronger signature
// than the three-byte "ID3" the stdlib uses for audio/mpeg. What makes it
// necessary at all is that RTF is printable ASCII, so the stdlib answers
// text/plain — an opinion, not the absence of one — and a .rtf was refused as
// a mismatch against its own type.
func validRTFStream(head []byte) bool {
	return bytes.HasPrefix(head, []byte(`{\rtf1`))
}
