package attachments

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"encoding/binary"
	"hash/crc32"
	"io"
)

// This file closes the second half of the gap BUG-2961 opened: the upload
// allowlist names types that SniffMIME cannot produce, so the door refuses —
// or silently retypes — files the allowlist says are supported. A measurement
// over real files (BUG-2963) found 25 of 48 allowlist entries unreachable as a
// stored type.
//
// WHAT THESE CHECKS DO AND DO NOT ESTABLISH. They RECOGNISE a format from its
// header, using the format's own structure rather than a magic-byte prefix.
// They do not PROVE the bytes are that format, and this is not a gap to be
// closed by adding more field checks — two review rounds walked that path to
// its end:
//
//   - Round 1 defeated prefix matching: a real, executing ELF binary with
//     "ustar" in unused padding at offset 257 was stored as application/x-tar.
//   - Round 2 defeated the checksum that was round 1's answer, with an ELF
//     carrying a correctly computed tar checksum. Measured here afterwards:
//     archive/tar's OWN Reader.Next accepts that file too. A 512-byte tar
//     header is exactly those fields, and nothing forbids an ELF's padding
//     from containing them, so the two are not distinguishable at this size —
//     by this package or by the standard library.
//
// That is a property of the formats, not a weakness in the code, and it is why
// the SAFETY property lives somewhere else entirely: whatever these bytes turn
// out to be, they are stored opaquely, never executed or decompressed by this
// server, and served back under a type from a reviewed allowlist with
// nosniff — as an attachment for every type recognised here. Content sniffing
// is heuristic by nature; the stdlib's own detector accepts a PNG signature
// followed by anything. What these checks buy is that ORDINARY files of a
// named type are recognised and ordinary files of other types are not.
//
// So each check uses the strongest reading available for its format — the
// standard library's real parser where one exists — and the comments say
// "recognises", never "establishes".
//
// No filename is trusted to introduce a type here. The one place an extension
// participates is documented at validADTSHeader, and it only DISAMBIGUATES a
// structure the bytes must already carry.

// sniffOpaqueMagic recognises formats the WHATWG mimesniff table has no
// signature for, so http.DetectContentType answers application/octet-stream
// for a real file of them.
//
// Called ONLY when the stdlib returned application/octet-stream. That ordering
// is about not overriding a detection the stdlib made; it is NOT what keeps
// these checks honest — the structural validation below is. Conflating the two
// is precisely the mistake the first version of this file made in a comment.
func sniffOpaqueMagic(head []byte) string {
	switch {
	case validSevenZipHeader(head):
		return "application/x-7z-compressed"
	case validFLACStream(head):
		return "audio/flac"
	case validBzip2Stream(head):
		return "application/x-bzip2"
	case validTarHeader(head):
		return "application/x-tar"
	}
	return ""
}

// validSevenZipHeader reports whether head opens a 7z archive: the six-byte
// signature, then a start header whose CRC32 the file carries and which must
// match. The CRC is what makes this structural — the signature alone is six
// bytes an attacker writes for free, while the CRC has to be computed over
// twenty bytes that then have to BE a start header.
//
// Layout (7z format specification, "Header" section):
//
//	 0..6   signature 37 7A BC AF 27 1C
//	 6..8   format version (major, minor) — not checked, versions move
//	 8..12  CRC32 of the next 20 bytes, little-endian
//	12..32  start header: NextHeaderOffset, NextHeaderSize (both uint64),
//	        NextHeaderCRC (uint32)
func validSevenZipHeader(head []byte) bool {
	const startHeader = 32
	if len(head) < startHeader || !bytes.HasPrefix(head, []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}) {
		return false
	}
	want := binary.LittleEndian.Uint32(head[8:12])
	if crc32.ChecksumIEEE(head[12:startHeader]) != want {
		return false
	}
	// A CRC proves the twenty bytes are the ones the writer intended; it says
	// nothing about whether they are POSSIBLE. A round-2 finding recomputed a
	// valid CRC over a header whose next-header position cannot be
	// represented, so the arithmetic is checked too.
	offset := binary.LittleEndian.Uint64(head[12:20])
	size := binary.LittleEndian.Uint64(head[20:28])
	end := offset + size + startHeader
	return end >= offset && end >= size
}

// validFLACStream reports whether head opens a native FLAC stream: the "fLaC"
// marker followed by a STREAMINFO metadata block of the one length the format
// permits.
//
// RFC 9639 §8.1-8.2: the first metadata block after the marker MUST be
// STREAMINFO (block type 0), and STREAMINFO's body is exactly 34 bytes. Both
// are fixed by the specification rather than by any encoder's choice, so
// checking them costs nothing and rejects a header that merely starts right.
func validFLACStream(head []byte) bool {
	const markerAndBlockHeader = 8
	const streamInfoLen = 34
	if len(head) < markerAndBlockHeader+streamInfoLen || !bytes.HasPrefix(head, []byte("fLaC")) {
		return false
	}
	// head[4]: high bit is the last-block flag, low seven bits the type.
	if head[4]&0x7F != 0 {
		return false
	}
	blockLen := uint32(head[5])<<16 | uint32(head[6])<<8 | uint32(head[7])
	if blockLen != streamInfoLen {
		return false
	}
	// The declaration is not the block. A round-2 finding accepted an
	// eight-byte file that PROMISED 34 bytes of STREAMINFO and supplied none,
	// and a 42-byte one whose STREAMINFO was all zeroes. So read the fields
	// the format constrains (RFC 9639 §8.2): block sizes are at least 16 and
	// ordered, and the sample rate may not be zero.
	info := head[markerAndBlockHeader : markerAndBlockHeader+streamInfoLen]
	minBlock := binary.BigEndian.Uint16(info[0:2])
	maxBlock := binary.BigEndian.Uint16(info[2:4])
	if minBlock < 16 || maxBlock < minBlock {
		return false
	}
	sampleRate := uint32(info[10])<<12 | uint32(info[11])<<4 | uint32(info[12])>>4
	return sampleRate != 0
}

// validBzip2Stream reports whether head opens a bzip2 stream, by DECODING it
// with the standard library.
//
// The header alone is four bytes plus a block magic, and a round-2 finding
// showed that a stream declaring itself empty could carry an invalid combined
// CRC and still pass a header-only test. Decoding is what reads the CRC, so
// this decodes — bounded, from the 512 bytes already in hand.
//
// It keeps the block-magic check as well; see the body for why neither the
// magic nor the decode subsumes the other.
func validBzip2Stream(head []byte) bool {
	if len(head) < 4 || !bytes.HasPrefix(head, []byte("BZh")) {
		return false
	}
	if head[3] < '1' || head[3] > '9' {
		return false
	}
	// The block magic, checked BEFORE decoding and kept alongside it. These
	// two catch different things and neither subsumes the other, which is what
	// makes both load-bearing rather than belt-and-braces:
	//
	//   - the magic refuses a stream too short for the decoder to judge —
	//     "BZh9\x00" is five bytes, and the decoder can only say "truncated";
	//   - the decode refuses an empty stream carrying an invalid combined CRC,
	//     which no header inspection reaches (round-2 finding).
	const prefixAndMagic = 10
	if len(head) < prefixAndMagic {
		return false
	}
	blockMagic := []byte{0x31, 0x41, 0x59, 0x26, 0x53, 0x59}     // pi
	streamEndMagic := []byte{0x17, 0x72, 0x45, 0x38, 0x50, 0x90} // sqrt(pi)
	if !bytes.Equal(head[4:prefixAndMagic], blockMagic) &&
		!bytes.Equal(head[4:prefixAndMagic], streamEndMagic) {
		return false
	}
	// Truncation is NOT a failure: this sees the head of a file, so a real
	// archive usually runs out mid-block and io.ErrUnexpectedEOF means "so
	// far, so good". Only a STRUCTURAL error refuses.
	_, err := io.ReadAll(io.LimitReader(bzip2.NewReader(bytes.NewReader(head)), 1<<16))
	return err == nil || err == io.ErrUnexpectedEOF
}

// validTarHeader reports whether head opens a tar archive, as read by the
// standard library's own tar reader.
//
// archive/tar is used rather than a hand-rolled magic-plus-checksum test for
// one reason: it is the same parser anything consuming the file would use, so
// this cannot drift from it, and it handles GNU and pax variants for free.
//
// It is NOT a stronger answer to the crafted-file question — measured: an ELF
// with "ustar" at 257 and a correctly computed checksum is accepted by
// Reader.Next as readily as by a hand-written check. See this file's header
// for why that is a property of the format rather than something to fix here.
func validTarHeader(head []byte) bool {
	const blockSize = 512
	// The length guard is CLARIFYING, not load-bearing: archive/tar refuses
	// anything shorter than a full 512-byte block on its own (measured — a
	// mutation lowering this to 262 changes no outcome). It stays because it
	// makes the slice below obviously in range at the point of reading.
	if len(head) < blockSize || !bytes.Equal(head[257:262], []byte("ustar")) {
		return false
	}
	_, err := tar.NewReader(bytes.NewReader(head)).Next()
	return err == nil
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
		if !ok || childSize > uint64(len(after)) {
			return ""
		}
		if childID == docTypeID {
			// DocType is an ASCII string whose value ENDS at the first NUL;
			// anything after that is padding and is not part of the value
			// (RFC 8794 §13). TrimRight was wrong here — a round-2 finding
			// produced an ffprobe-readable Matroska whose DocType payload was
			// "matroska\x00junk", which trimming left intact and so failed to
			// match, storing a real Matroska as WebM.
			value := after[:childSize]
			if i := bytes.IndexByte(value, 0); i >= 0 {
				value = value[:i]
			}
			switch string(value) {
			case "matroska":
				return "video/x-matroska"
			case "webm":
				return "video/webm"
			}
			// A DocType we do not recognise is not ours to name; the stdlib's
			// answer is no worse than a guess.
			return ""
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

// validADTSHeader reports whether head opens a syntactically valid ADTS frame
// header — the framing raw .aac files use.
//
// The SYNC ALONE IS NOT A SIGNATURE: twelve set bits at offset zero, which
// MPEG audio shares, and an adversarial round showed the three-byte body
// FF F1 00 being stored as audio/aac by a check that looked no further. So
// this reads the fields that carry reserved values — the layer bits, which
// ADTS requires to be zero, and the sampling-frequency index, whose values 13
// through 15 are reserved — plus the frame-length field, which cannot describe
// a frame shorter than the header it sits in.
//
// Even so this remains the weakest check in the file, because everything it
// reads is small and dense. That is why the caller ALSO gates it on the .aac
// extension (see ValidateUpload): not because the name is evidence, but
// because a weak structure should not be allowed to speak unprompted.
//
// Layout (ISO/IEC 13818-7 §6.2, adts_fixed_header + adts_variable_header):
//
//	byte 0      syncword high 8 bits
//	byte 1      sync low 4 | MPEG version | layer (2 bits) | protection absent
//	byte 2      profile (2) | sampling frequency index (4) | private | ch high
//	bytes 3..5  channel config, flags, frame length (13 bits)
func validADTSHeader(head []byte) bool {
	const headerBytes = 7
	if len(head) < headerBytes {
		return false
	}
	if head[0] != 0xFF || head[1]&0xF0 != 0xF0 {
		return false
	}
	if head[1]>>1&0x03 != 0 { // layer MUST be 00 for ADTS
		return false
	}
	if idx := head[2] >> 2 & 0x0F; idx > 12 { // 13, 14, 15 are reserved
		return false
	}
	// Bit 0 of byte 1 is protection_absent: when it is CLEAR a two-byte CRC
	// follows the header, so the frame cannot be shorter than nine bytes. A
	// round-2 finding passed a frame declaring seven bytes with the CRC flag
	// set — too short for its own header.
	minFrame := uint32(headerBytes)
	if head[1]&0x01 == 0 {
		minFrame += 2
	}
	frameLen := uint32(head[3]&0x03)<<11 | uint32(head[4])<<3 | uint32(head[5])>>5
	return frameLen >= minFrame
}

// Ogg is deliberately NOT recognised here, and this comment is the record of
// why, because "add an application/ogg alias" is the obvious thing for the
// next reader to try (BUG-2963 F1; ruled in, then ruled out again after
// review).
//
// The stdlib names Ogg by its container (application/ogg); the allowlist names
// the payload it reviewed (audio/ogg). Bridging the two requires deciding that
// a file is AUDIO, and that question cannot be answered from the head of the
// file:
//
//   - An unconditional alias admits Ogg video. A real VP8-in-Ogg file was
//     stored as audio/ogg, category audio, and served INLINE.
//   - Gating on the first packet's codec does not fix it. An Ogg with an Opus
//     stream first and a VP8 stream second passes, because Ogg multiplexes and
//     the second stream's pages come later in the file — past anything a
//     512-byte sniff can see.
//   - The gate is also wrong in the other direction: a legitimate
//     Skeleton-prefixed Ogg audio file leads with "fishead\x00" and would be
//     refused, and a codec check that matches only an identification magic
//     accepts an eight-byte packet carrying no identification fields at all.
//
// So Ogg stays refused, exactly as it was before this change — no regression,
// and no acceptance the allowlist never reviewed. Making Ogg work means either
// adding video/ogg to the allowlist as a reviewed type, or demuxing far enough
// to enumerate the streams. Both are decisions of their own, and neither
// belongs in a change whose whole premise is that it adds no new trust.
