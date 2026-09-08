package attachments

import (
	"bytes"
	"encoding/binary"
)

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

var (
	isoBMFFFtypBox = []byte("ftyp")
	isoBMFFMP4Pfx  = []byte("mp4")
)

// sniffISOBMFFImage returns the allowlisted image MIME for an ISO-BMFF still
// image, or "" for anything else — including a valid ISO-BMFF file that is not
// a still image. "" means "I have no opinion, ask the stdlib", so this can only
// ever ADD detections; it never overrides one http.DetectContentType would make
// on the same bytes, with one deliberate exception noted below.
//
// head may be a prefix of the file (SniffMIME passes at most 512 bytes). The
// ftyp box is the first box in the file and is small — 28 bytes for the AVIF in
// testdata, 24 for the HEIF — so a 512-byte prefix carries it whole in practice;
// where it does not, the scan is bounded by what is present rather than reading
// past it.
func sniffISOBMFFImage(head []byte) string {
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
