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
func DroppedFilenameRune(r rune) bool {
	return unicode.IsControl(r) || r == '"' || r == '\\'
}

// fallbackFilename is the name for an upload whose own name leaves nothing.
const fallbackFilename = "upload.bin"

// NormalizeFilename is the stored form of a caller-supplied attachment name.
// Every door that writes attachments.filename calls it, before ValidateUpload
// sees the name. It normalises rather than refuses: the bytes of the upload are
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
	return name
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
