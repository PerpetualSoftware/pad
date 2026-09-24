package attachments

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// BUG-2818: one filename, three readers.
//
// A stored attachment name is read by the extension blocklist (ValidateUpload),
// by the Content-Disposition header that names the download, and by whatever
// saves that download to disk. They used to disagree: the header dropped
// control bytes, quotes and backslashes that the blocklist had counted as part
// of the extension, so "x.s<VT>vg" or `x.s"vg` passed the blocklist (".s<VT>vg"
// is no known extension) and reached the client as "x.svg". The blocklist is the
// whole defence for SVG, whose bytes sniff as allowed text/xml.
//
// The fix is that all three read ONE string. NormalizeFilename runs at every
// ingest door BEFORE ValidateUpload, and the header drops characters with the
// same predicate, DroppedFilenameRune, so the name the blocklist evaluates is by
// construction the name the header serves.

// DroppedFilenameRune reports whether r is removed from an attachment filename.
// It is the ONE character rule shared by NormalizeFilename (ingest) and the
// Content-Disposition sanitiser (serve); a second copy of it anywhere is the
// defect this exists to prevent.
//
//   - Unicode control characters (C0, DEL and C1): no legitimate use in a name,
//     and the header cannot carry them.
//   - '"' and '\': they break the header's quoted-string, so the header has to
//     drop them, and anything it drops the blocklist must not have seen.
//   - The Unicode Bidi_Control property (BUG-3153): the embeddings, overrides
//     and isolates (U+202A-U+202E, U+2066-U+2069) and the implicit marks
//     (U+200E, U+200F, U+061C). They make a bidi-aware renderer DISPLAY an
//     extension the name does not have, so "x<U+202E>gvs.txt" (a .txt) shows as
//     "xtxt.svg". Chosen by property rather than by range so the set is the
//     standard's; ZWJ is Join_Control, not Bidi_Control, and emoji sequences
//     keep it. One inside an extension also hides it from the blocklist, as a
//     control byte did: "x.s<U+202E>vg" normalises to "x.svg" and is refused.
func DroppedFilenameRune(r rune) bool {
	return unicode.IsControl(r) || r == '"' || r == '\\' || unicode.Is(unicode.Bidi_Control, r)
}

// fallbackFilename is the name for an upload whose own name leaves nothing.
const fallbackFilename = "upload.bin"

// NormalizeFilename is the stored form of a caller-supplied attachment name.
// Every door that writes a CALLER-SUPPLIED name calls it (upload, bundle
// import), before ValidateUpload sees the name. Derived rows (thumbnails,
// transforms, cross-workspace copies) take their name from a row that already
// went through it; a legacy row that did not is rewritten at startup when it
// carries a Bidi_Control character (store.BackfillBidiAttachmentFilenames). It normalises rather than refuses: the bytes of the upload are
// fine and only the label is at issue, and an attack name still ends in a
// refusal, because its normalised form carries the extension it was hiding
// (checkpoint 1 on BUG-2818 has the full reasoning).
//
// In order:
//  1. Reduce to a leaf under BOTH separator conventions. filepath.Base is
//     platform-specific and leaves a backslash alone on Unix, while the stored
//     name is consumed cross-platform, where "..\evil" is a traversal.
//  2. A name that is not valid UTF-8, or that carries a NUL, cannot be stored
//     (the server's bindableText rule; BUG-2803's fallback). It falls back to
//     "upload" plus its extension only when that extension is known and allowed
//     (SafeFallbackExtension), so the fallback cannot carry a blocked suffix.
//  3. Drop every rune DroppedFilenameRune names.
//  4. Trim trailing dots and spaces. Windows and browser download sanitisers
//     strip them when saving, so "x.svg." lands on disk as "x.svg" while
//     filepath.Ext reads ".". Skipped when it would leave nothing.
//  5. A name left empty, or that is only a path component ("." or ".."),
//     becomes "upload.bin".
//  6. A Windows reserved device name gets a "_" prefix (BUG-2822): Windows
//     opens "nul.txt" as the null device, so a download saved under the stored
//     name is lost or refused. See WindowsDeviceName for the match.
func NormalizeFilename(raw string) string {
	name := filepath.Base(raw)
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		name = name[i+1:]
	}
	if !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		if ext := filepath.Ext(name); SafeFallbackExtension(ext) {
			return "upload" + strings.ToLower(ext)
		}
		return "upload"
	}
	name = strings.Map(func(r rune) rune {
		if DroppedFilenameRune(r) {
			return -1
		}
		return r
	}, name)
	// Only when something is left: a name that is ALL dots and spaces has no
	// extension to expose, and "..." is an ordinary POSIX name (BUG-2803,
	// codex round 27), while "." and ".." are caught below as path components.
	if trimmed := strings.TrimRight(name, ". "); trimmed != "" {
		name = trimmed
	}
	if name == "" || name == "." || name == ".." || name == "/" {
		return fallbackFilename
	}
	if WindowsDeviceName(name) {
		return "_" + name
	}
	return name
}

// windowsReservedStems are the device names Windows resolves a path component
// to, whatever directory it sits in: Microsoft's "Naming Files, Paths, and
// Namespaces" list, plus CONIN$ and CONOUT$, and the superscript digits that
// list names for COM and LPT. Upper case; the match folds case.
var windowsReservedStems = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true}
	for _, d := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00B9", "\u00B2", "\u00B3"} {
		m["COM"+d] = true
		m["LPT"+d] = true
	}
	return m
}()

// WindowsDeviceName reports whether Windows would open name as a device. It
// reads the name as Windows does: the stem is everything before the FIRST dot
// or colon, so "con.tar.gz" and "nul:x.txt" are devices (a colon opens the
// stream syntax, codex round 1), and trailing spaces in the stem are ignored,
// so "con .txt" is one too. The match folds case.
//
// Exported for BUG-3185: every name offered for download or written by the
// CLI that is derived from user data goes through this one rule. Do not write
// a second predicate; the stem rule has already been corrected once.
func WindowsDeviceName(name string) bool {
	stem := name
	if i := strings.IndexAny(name, ".:"); i >= 0 {
		stem = name[:i]
	}
	return windowsReservedStems[strings.ToUpper(strings.TrimRight(stem, " "))]
}

// windowsReservedChars are the characters Windows refuses in a path
// component. The separators are included for completeness; callers that have
// already stripped directories never meet them. ':' is the dangerous one:
// "a.svg:x.txt" is not refused, it names an NTFS alternate data stream, so the
// bytes land in a stream on an empty "a.svg" (BUG-3186).
const windowsReservedChars = `<>:"/\|?*`

// WindowsPathComponent returns name made safe to use as ONE path component on
// Windows (BUG-3186): each reserved character and each C0 control becomes "_",
// trailing dots and spaces (which Windows strips silently) are trimmed, and a
// device stem gets the "_" prefix WindowsDeviceName calls for. It returns ""
// when nothing usable survives, so callers keep their own fallback.
//
// It is for the boundary where a stored name becomes a PATH, never for the
// stored name itself: "Meeting: notes.pdf" is a normal name to store and to
// show (the lead ruling on BUG-3186). Apply it where the path is made, and on
// the platform it describes.
func WindowsPathComponent(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(windowsReservedChars, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimRight(b.String(), ". ")
	if out == "" {
		return ""
	}
	if WindowsDeviceName(out) {
		return "_" + out
	}
	return out
}

// ServedFilename is the name an attachment is offered under, derived from its
// STORED name. For a row stored after BUG-2818 it equals the stored name.
// For a LEGACY row, stored before ingest normalised, it applies the same
// normalisation, and when that uncovers an extension the blocklist refuses
// (the row that was never evaluated as one), it replaces the extension with
// ".bin" so the download cannot land as the blocked type.
func ServedFilename(stored string) string {
	name := NormalizeFilename(stored)
	if ext := filepath.Ext(name); BlockedExtension(ext) {
		stem := strings.TrimSuffix(name, ext)
		if stem == "" {
			stem = "attachment"
		}
		return stem + ".bin"
	}
	return name
}
