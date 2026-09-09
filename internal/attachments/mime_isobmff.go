package attachments

import (
	"bytes"
	"encoding/binary"
)

// This file recognises ISO base media file format (ISO/IEC 14496-12) files the
// standard library's sniffer cannot type: still images, which is what it was
// written for (BUG-2961), and two audio/video major brands added by BUG-2963
// F4 and argued at isoBMFFAVBrands.
//
// http.DetectContentType implements the WHATWG mimesniff table, which has no
// signature for ISO base media file format (ISO/IEC 14496-12) still images:
// HEIC, HEIF and AVIF all sniff as application/octet-stream. All three are on
// the `allowed` map, so before this pre-check every real file of those types
// was rejected 415 mime_not_allowed by ValidateUpload's first rule — the
// allowlist claimed a support the door could not deliver (BUG-2961). HEIC is
// the iPhone camera default, so that is every photo shared straight from an
// Apple device.
//
// The fix stays inside rule 1 rather than around it: we recognise the type
// FROM THE BYTES, exactly as the stdlib detector does for every other format,
// and the sniffed result is still what the allowlist is consulted with. The
// alternative — trusting the filename extension for these three, the way
// ValidateUpload already trusts it for zip-based Office documents — was
// rejected in triage: a renamed .heic would then pass on a name alone.
//
// Structure of the box this reads (14496-12 §4.3):
//
//	 0..4   uint32 box size, big-endian, includes this header
//	 4..8   "ftyp"
//	 8..12  major brand
//	12..16  minor version (ignored)
//	16..    compatible brands, four bytes each, to the end of the box

// isoBMFFStillImageBrands maps the brands that denote a SINGLE STILL IMAGE to
// the MIME the allowlist spells. Sequence brands are deliberately absent —
// 'hevc'/'hevx'/'hevm'/'hevs' (HEVC image sequences), 'avis' (AVIF sequence)
// and 'msf1' (the generic image-sequence brand) name image/heic-sequence,
// image/avif-sequence and image/heif-sequence, and NONE of those three is on
// the allowlist. Leaving them out means such a file falls through to the
// stdlib detector and is refused, which is what a default-deny allowlist
// should do with a type nobody has reviewed. Adding one here without adding
// it to `allowed` would do nothing; adding it to both is a security decision,
// same as any other allowlist entry.
//
// Brands per ISO/IEC 23008-12 Annex B (HEIF) and the AV1 Image File Format
// specification §4.
var isoBMFFStillImageBrands = map[string]string{
	"heic": "image/heic", // HEVC still image, Main profile — the iPhone default
	"heix": "image/heic", // HEVC still image, Main 10 / Main Still 4:2:2
	"heim": "image/heic", // multiview HEVC still image
	"heis": "image/heic", // scalable HEVC still image
	"avif": "image/avif", // AV1 still image
}

// isoBMFFGenericImageBrand is the codec-agnostic HEIF still-image brand. A file
// carrying only this one says "I am a HEIF image" without naming its codec, so
// it maps to the generic image/heif. It is consulted only after every brand has
// been checked against isoBMFFStillImageBrands, because encoders emit it
// ALONGSIDE the specific brand — the AVIF this package's tests are built from
// has major brand 'avif' and compatible brands mif1, avif, miaf, and a
// mif1-major file whose compatible list carries 'avif' is an AVIF, not an
// unspecified HEIF. Preferring the specific brand wherever one appears keeps
// the stored MIME the most truthful one available.
const (
	isoBMFFGenericImageBrand     = "mif1"
	isoBMFFGenericImageBrandMIME = "image/heif"
)

// isoBMFFAVBrands maps MAJOR brands that name an audio or video container to
// the MIME the allowlist spells. Consulted on the major brand ALONE, and only
// on these two, which is what keeps it narrow enough to be safe (BUG-2963 F4,
// ruled day 62).
//
//   - "qt  " is the QuickTime brand. The stdlib has no signature for it: its
//     mp4 matcher wants a brand beginning "mp4" and a .mov carries none, so a
//     real QuickTime file sniffs application/octet-stream and was refused
//     mime_not_allowed while video/quicktime sat on the allowlist. Recognising
//     it ADDS a detection to a verdict of "no opinion", exactly as the
//     still-image brands do.
//
//   - "M4A " is the MPEG-4 AUDIO brand, and it is the one place this package
//     OVERRIDES a type the standard library identified. An .m4a's compatible
//     brands routinely include "mp41", so the stdlib answers video/mp4 — from
//     a COMPATIBLE brand, having no way to weigh it against the major one.
//     The major brand is the file's own statement of what it is, and a file
//     that says M4A is audio. The override is deliberate and it is bounded:
//     both types are on the allowlist, both render inline, and the only thing
//     that moves is the CATEGORY, which decides whether the UI offers an audio
//     player or a video one. Nothing here decides how a file is executed or
//     decompressed, because nothing decompresses or executes it.
//
// Sequence and still-image brands are not here; they are handled below.
// Anything not in this map falls through to the existing logic, so adding a
// brand is a decision about one brand and nothing else.
var isoBMFFAVBrands = map[string]string{
	"qt  ": "video/quicktime",
	"M4A ": "audio/mp4",
}

var (
	isoBMFFFtypBox = []byte("ftyp")
	isoBMFFMP4Pfx  = []byte("mp4")
)

// sniffISOBMFF returns the allowlisted MIME for an ISO-BMFF file this package
// recognises — a still image by any of its brands, or an audio/video container
// by its MAJOR brand — and "" for anything else, including a valid ISO-BMFF
// file that is neither. "" means "I have no opinion, ask the stdlib".
//
// For every brand but one this can only ADD detections and never overrides a
// type http.DetectContentType would name on the same bytes; the mp4-yield
// below is what preserves that for the shape where both could match. The one
// exception is the "M4A " major brand, which is deliberate and argued at
// isoBMFFAVBrands. This function was called sniffISOBMFFImage until BUG-2963
// F4 gave it audio and video to answer for.
//
// head may be a prefix of the file (SniffMIME passes at most 512 bytes). The
// ftyp box is the first box in the file and is small — 28 bytes for the AVIF in
// testdata, 24 for the HEIF — so a 512-byte prefix carries it whole in practice;
// where it does not, the scan is bounded by what is present rather than reading
// past it.
func sniffISOBMFF(head []byte) string {
	// Sixteen, not twelve: a complete ftyp header is 8 bytes of box header plus
	// a major brand plus a minor version, so a buffer that stops inside it is a
	// truncated file rather than a small one. Classifying from a 13-byte buffer
	// would let "00 00 00 10 ftyp heic" — three fields of a box that does not
	// exist — be stored and served as an image (codex round 2, P2). Nothing
	// legitimate is lost: no ISO-BMFF still image is 15 bytes long, and
	// SniffMIME hands this the first 512 bytes of the upload.
	if len(head) < 16 || !bytes.Equal(head[4:8], isoBMFFFtypBox) {
		return ""
	}

	// Bound the compatible-brand scan by the box size when it is present and
	// sane, and by the buffer otherwise. A truncated head is the ordinary
	// case for a large ftyp box, and it is not a reason to refuse to read the
	// major brand we DO have — unlike the stdlib's mp4 matcher, which bails
	// when the box runs past the buffer. Bounding matters in the other
	// direction too: without it the scan would run past the ftyp box into
	// whatever follows and could read four bytes of payload as a brand.
	//
	// A declared size below 16 is not a box this function can read, and the
	// two reasons are different. Size 1 means a 64-bit largesize follows the
	// type field (14496-12 §4.2), shifting every brand eight bytes along, so
	// the fixed offsets below would be reading the size itself; no encoder
	// writes a largesize ftyp box, which is a few dozen bytes. Sizes 2..15 are
	// simply impossible — 8 bytes of header plus a major brand plus a minor
	// version is already 16 — so the bytes at 8.. are not brands of THIS box
	// whatever else they are. Both decline: a payload that opens with a size
	// of 8 and the letters ftyp heic must not be classified from bytes its own
	// header says are not in the box (codex round 1, P2).
	//
	// Size 0 is the one small value that IS legal: it means "to the end of the
	// file" and shifts nothing.
	end := len(head)
	boxSize := int(binary.BigEndian.Uint32(head[:4]))
	if boxSize != 0 && boxSize < 16 {
		return ""
	}
	if boxSize >= 16 && boxSize < end {
		end = boxSize
	}

	// The AUDIO/VIDEO brands are read from the MAJOR brand only, and BEFORE the
	// mp4-yield below, because "M4A " files list "mp41" among their compatible
	// brands and the yield would return "" before this could speak. That
	// ordering is the whole mechanism, so it is stated rather than left to be
	// rediscovered: see isoBMFFAVBrands for why one of these two overrides the
	// stdlib and the other does not.
	if mime, ok := isoBMFFAVBrands[string(head[8:12])]; ok {
		return mime
	}

	// A video container that happens to list one of our brands stays a video:
	// yield to the stdlib, which detects MP4 from the same box. No real HEIF
	// or AVIF carries an mp4* brand, so this costs nothing in the cases the
	// pre-check exists for, and it keeps the "never override the stdlib"
	// property true for the one shape where both could match.
	for st := 8; st+4 <= end; st += 4 {
		if st == 12 {
			continue // minor version, not a brand
		}
		if bytes.HasPrefix(head[st:st+4], isoBMFFMP4Pfx) {
			return ""
		}
	}

	generic := false
	for st := 8; st+4 <= end; st += 4 {
		if st == 12 {
			continue // minor version, not a brand
		}
		brand := string(head[st : st+4])
		if mime, ok := isoBMFFStillImageBrands[brand]; ok {
			return mime
		}
		if brand == isoBMFFGenericImageBrand {
			generic = true
		}
	}
	if generic {
		return isoBMFFGenericImageBrandMIME
	}
	return ""
}
