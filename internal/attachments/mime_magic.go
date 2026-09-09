package attachments

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
)

// This file closes the second half of the gap BUG-2961 opened: the upload
// allowlist names types that SniffMIME cannot produce, so the door refuses —
// or silently retypes — files the allowlist says are supported. A measurement
// over real files (BUG-2963) found 25 of 48 allowlist entries unreachable as a
// stored type.
//
// EVERY CHECK HERE VALIDATES STRUCTURE, NOT A PREFIX. That is the whole design
// and it was learned the hard way: the first version of this file matched
// magic bytes and nothing else, and an adversarial round walked straight
// through it — a real, executing ELF binary with the five bytes "ustar"
// sitting in unused padding at offset 257 was stored as application/x-tar, a
// six-byte body was a 7z archive, and "fLaC\x00" was a FLAC stream. Each of
// those was REFUSED before the change and accepted after it, which means a
// prefix matcher does not make an allowlisted type reachable — it makes the
// allowlist porous, on a door whose entire posture is default-deny.
//
// So each format is admitted only by the integrity check the format itself
// defines: tar's header checksum, bzip2's block magic, 7z's start-header CRC,
// FLAC's mandatory STREAMINFO block, ADTS's reserved field values. These are
// cheap — all of them read the first 512 bytes we already have — and they are
// the difference between "these bytes begin like an X" and "these bytes are
// the beginning of an X".
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
	return crc32.ChecksumIEEE(head[12:startHeader]) == want
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
	if len(head) < markerAndBlockHeader || !bytes.HasPrefix(head, []byte("fLaC")) {
		return false
	}
	// head[4]: high bit is the last-block flag, low seven bits the type.
	if head[4]&0x7F != 0 {
		return false
	}
	blockLen := uint32(head[5])<<16 | uint32(head[6])<<8 | uint32(head[7])
	return blockLen == 34
}

// validBzip2Stream reports whether head opens a bzip2 stream: "BZh", the
// block-size digit, and then one of the two 48-bit magics the format defines.
//
// A bzip2 stream is "BZh" + '1'..'9' + a sequence of blocks, and every block
// begins with the compressed-block magic (the digits of pi); a stream with no
// blocks at all begins with the end-of-stream magic (the digits of the square
// root of pi) instead. There is no third possibility, so a file that has "BZh9"
// and then anything else is not a bzip2 stream — which is what the previous
// version of this check accepted.
func validBzip2Stream(head []byte) bool {
	const prefixAndMagic = 10
	if len(head) < prefixAndMagic || !bytes.HasPrefix(head, []byte("BZh")) {
		return false
	}
	if head[3] < '1' || head[3] > '9' {
		return false
	}
	blockMagic := []byte{0x31, 0x41, 0x59, 0x26, 0x53, 0x59}     // pi
	streamEndMagic := []byte{0x17, 0x72, 0x45, 0x38, 0x50, 0x90} // sqrt(pi)
	return bytes.Equal(head[4:prefixAndMagic], blockMagic) ||
		bytes.Equal(head[4:prefixAndMagic], streamEndMagic)
}

// validTarHeader reports whether head opens a POSIX ustar archive: the magic
// at offset 257 AND a header checksum that verifies.
//
// The checksum is the point. "ustar" is five bytes at a fixed offset, and an
// adversarial round demonstrated a working ELF executable carrying them in
// padding — accepted as a tar by a matcher that stopped at the magic. The
// checksum is a sum over all 512 header bytes, so it cannot be satisfied
// incidentally; a file that passes it has a tar header, not a coincidence.
//
// Both the unsigned and the signed sum are accepted, matching archive/tar:
// historical implementations disagreed about whether the bytes are signed, and
// real archives in the wild carry both.
func validTarHeader(head []byte) bool {
	const blockSize = 512
	if len(head) < blockSize || !bytes.Equal(head[257:262], []byte("ustar")) {
		return false
	}
	want, ok := parseTarOctal(head[148:156])
	if !ok {
		return false
	}
	var unsigned, signed int64
	for i := 0; i < blockSize; i++ {
		b := head[i]
		// The checksum field itself is read as spaces, per the format —
		// otherwise the sum would depend on the value being computed.
		if i >= 148 && i < 156 {
			b = ' '
		}
		unsigned += int64(b)
		signed += int64(int8(b))
	}
	return want == unsigned || want == signed
}

// parseTarOctal reads tar's octal-ASCII number fields, which are padded with
// spaces or NULs on either side and may be terminated by either.
func parseTarOctal(field []byte) (int64, bool) {
	field = bytes.Trim(field, " \x00")
	if len(field) == 0 {
		return 0, false
	}
	var n int64
	for _, c := range field {
		if c < '0' || c > '7' {
			return 0, false
		}
		n = n*8 + int64(c-'0')
	}
	return n, true
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
			// DocType is an ASCII string, NUL-padded to its declared length.
			switch string(bytes.TrimRight(after[:childSize], "\x00")) {
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
	return id, b[n:], true
}

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
	frameLen := uint32(head[3]&0x03)<<11 | uint32(head[4])<<3 | uint32(head[5])>>5
	return frameLen >= headerBytes
}

// sniffOggAudio returns audio/ogg for an Ogg stream whose first packet
// identifies an AUDIO codec, and "" for anything else in an Ogg container.
//
// The stdlib names Ogg by its container (application/ogg); the allowlist names
// the payload it reviewed (audio/ogg). A plain spelling alias between the two
// was the first attempt and it was wrong in a way worth recording: an
// adversarial round uploaded a real VP8-in-Ogg file as clip.ogv and the door
// stored it as audio/ogg, category audio, SERVED INLINE. Ogg carries video and
// applications as readily as audio, so aliasing the container name to an audio
// type makes a video format the allowlist never reviewed uploadable — which is
// the one thing a default-deny list exists to prevent (BUG-2963 F1, re-ruled
// day 62 after that finding).
//
// So the alias is conditional on the codec. Theora and VP8 in Ogg stay refused
// exactly as they are today; video/ogg is deliberately NOT added to the
// allowlist, because admitting a format is a review, not a side effect.
//
// Ogg page layout (RFC 3533 §6): a 27-byte header, then one length byte per
// segment, then the packet data — so the first packet's identification header,
// which every Ogg codec puts first, sits at a computable offset.
func sniffOggAudio(head []byte) string {
	const pageHeader = 27
	if len(head) < pageHeader || !bytes.HasPrefix(head, []byte("OggS")) {
		return ""
	}
	segments := int(head[pageHeader-1])
	packet := pageHeader + segments
	if packet >= len(head) {
		return ""
	}
	first := head[packet:]
	// The identification headers the audio codecs on the allowlist emit.
	for _, magic := range [][]byte{
		[]byte("\x01vorbis"), // RFC 5334 — Vorbis
		[]byte("OpusHead"),   // RFC 7845 §5.1 — Opus
		[]byte("\x7fFLAC"),   // RFC 9639 §10.1 — FLAC in Ogg
		[]byte("Speex   "),   // Speex identification header
	} {
		if bytes.HasPrefix(first, magic) {
			return "audio/ogg"
		}
	}
	return ""
}
