package attachments

import "bytes"

// This file closes the second half of the gap BUG-2961 opened: the upload
// allowlist names types that SniffMIME cannot produce, so the door refuses
// files the allowlist says are supported. A measurement over real files
// (BUG-2963, 44 files / 41 extensions) found 25 of 48 allowlist entries
// unreachable as a stored type. What follows fixes the sniff-side half of
// that — types the allowlist ALREADY names become reachable from their own
// bytes. No filename is trusted to choose a type here; the one place an
// extension participates is documented at adtsAAC, and it only DISAMBIGUATES
// a signature the bytes must already carry.
//
// The trust-side half — extension trust for the text family, the audio/video
// category split, and the CFB office trio — is deliberately NOT here.

// sniffOpaqueMagic recognises formats the WHATWG mimesniff table has no
// signature for at all, so http.DetectContentType answers
// application/octet-stream for every real file of them.
//
// It is called ONLY when the stdlib has already returned
// application/octet-stream, which is what makes it strictly additive: it can
// turn "no opinion" into an opinion, and it can never override a detection
// the stdlib actually made. That ordering matters more than the individual
// signatures — a pre-check that ran unconditionally could retype a PNG whose
// bytes happened to collide, and none of these formats is worth that risk.
//
// Every signature here is the format's own defining magic, and each was
// verified against a file produced by a real encoder rather than typed from a
// specification (BUG-2963's measurement pass: py7zr for 7z, libsndfile for
// FLAC, GNU tar and bzip2 for the other two).
func sniffOpaqueMagic(head []byte) string {
	switch {
	// 7-Zip: six-byte signature, format-defining, no shorter prefix in use.
	case bytes.HasPrefix(head, []byte{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}):
		return "application/x-7z-compressed"

	// FLAC: the four-byte "fLaC" stream marker opens every native FLAC
	// stream (RFC 9639 §8.1).
	case bytes.HasPrefix(head, []byte("fLaC")):
		return "audio/flac"

	// bzip2: "BZh" followed by the block-size digit '1'..'9'. The digit is
	// part of the check on purpose — "BZh" alone is three common ASCII
	// bytes, and requiring the digit makes an accidental match need a
	// four-byte coincidence at offset zero instead of a three-byte one.
	case len(head) >= 4 && bytes.HasPrefix(head, []byte("BZh")) &&
		head[3] >= '1' && head[3] <= '9':
		return "application/x-bzip2"

	// tar: POSIX ustar puts the magic at offset 257, not at the start — a
	// tar's first bytes are the member filename, which is why the stdlib
	// (and any prefix-only matcher) has nothing to go on. Both the POSIX
	// spelling "ustar\0" and the GNU one "ustar  \0" begin with these five
	// bytes, so matching the five covers both without enumerating them.
	case len(head) >= 262 && bytes.Equal(head[257:262], []byte("ustar")):
		return "application/x-tar"
	}
	return ""
}

// sniffEBMLDocType refines the stdlib's WebM verdict using the EBML header's
// DocType.
//
// The mimesniff table maps the bare EBML magic (1A 45 DF A3) to video/webm
// with no DocType check, so a Matroska file — a different container, on the
// allowlist under its own name — is ACCEPTED and stored as video/webm. That
// is not a refusal, which is why it survived the first pass at BUG-2963: the
// upload succeeds and the stored type is simply false, and a player handed a
// .mkv labelled WebM may fail to decode it.
//
// Called only when the stdlib returned video/webm, so it can only ever move a
// file between the two EBML types, never out of them.
//
// The DocType is a string in the EBML header, which by specification precedes
// all cluster data; in files from real muxers it sits around offset 24. This
// searches the leading window rather than parsing EBML elements: a full
// element walk buys nothing here, because the only question is which of two
// fixed strings is present. If neither is in the window — a header padded far
// past anything a muxer emits — the stdlib verdict stands, so the failure
// mode is today's behaviour rather than a refusal.
func sniffEBMLDocType(head []byte) string {
	if !bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		return ""
	}
	window := head
	if len(window) > ebmlDocTypeWindow {
		window = window[:ebmlDocTypeWindow]
	}
	// "matroska" first: it is the longer, more specific string, and a
	// DocType carries exactly one of the two, so order cannot change any
	// answer for a well-formed file. It is fixed here so a malformed one
	// gets a deterministic answer rather than a map-order-dependent one.
	if bytes.Contains(window, []byte("matroska")) {
		return "video/x-matroska"
	}
	if bytes.Contains(window, []byte("webm")) {
		return "video/webm"
	}
	return ""
}

// ebmlDocTypeWindow bounds the DocType search. Measured on files from the
// two muxers to hand (BUG-2963): the DocType string starts at offset 24 in
// both a Matroska and a WebM file. 64 leaves that room to roughly double
// before this stops firing, and when it stops firing the result is the
// stdlib's answer — the behaviour that shipped before this file existed.
const ebmlDocTypeWindow = 64

// hasADTSSync reports whether head opens with an ADTS frame header — the
// framing raw .aac files use.
//
// This signature is WEAK: twelve bits of sync plus two layer bits, at offset
// zero, with no magic string anywhere. Twelve set bits are not rare in
// arbitrary binary, and MPEG audio shares the same syncword shape, so on its
// own this would be a real false-positive risk on a default-deny allowlist.
//
// So the caller gates it on the .aac EXTENSION as well (see ValidateUpload).
// That is a narrower thing than it looks: the extension cannot introduce a
// type on its own — the bytes must still carry the sync — it only decides
// whether a weak signature is allowed to speak. A .png whose bytes open with
// an ADTS sync is unaffected, and a file renamed to .aac without the sync is
// still refused.
func hasADTSSync(head []byte) bool {
	// Byte 0 all ones and byte 1's top four bits set give the 12-bit sync;
	// bits 2..1 of byte 1 are the MPEG layer, which ADTS requires to be 00.
	// Masking both in one test (0xF6) keeps the two conditions inseparable.
	return len(head) >= 2 && head[0] == 0xFF && head[1]&0xF6 == 0xF0
}
