package attachments

import (
	"net/http"
	"path/filepath"
	"strings"
)

// RenderMode controls how the browser presents a successfully-served
// attachment. It is a server-side decision (not a client preference) so
// it travels with the MIME entry, not with the request.
type RenderMode int

const (
	// RenderInline means the browser renders the response in place
	// (image, audio, video). Content-Disposition is "inline".
	RenderInline RenderMode = iota
	// RenderChip means the editor displays a download chip instead of
	// embedding the bytes (PDFs, archives, office docs). The HTTP layer
	// still serves these inline so the browser can preview if it wants
	// to (PDF in particular); the Content-Disposition stays "inline".
	RenderChip
	// RenderForceDownload means we set Content-Disposition: attachment
	// to keep the browser from interpreting the bytes inline. This is
	// the safety bucket for HTML/JS/CSS/text/* — bytes that would XSS
	// if served inline.
	RenderForceDownload
)

// Category drives the icon shown on file chips and is also used by
// quota/usage UI to bucket totals (Phase 2 will surface "you have 1.2GB
// of images and 400MB of documents"). Keep values short and stable —
// they're effectively API surface for the editor.
type Category string

const (
	CategoryImage    Category = "image"
	CategoryVideo    Category = "video"
	CategoryAudio    Category = "audio"
	CategoryDocument Category = "document"
	CategoryText     Category = "text"
	CategoryArchive  Category = "archive"
	CategoryOther    Category = "other"
)

// MIMEEntry describes how Pad treats one MIME type.
type MIMEEntry struct {
	MIME       string
	RenderMode RenderMode
	Category   Category
}

// allowed is the MIME allowlist from DOC-865.
//
// IMPORTANT: this is a default-deny list. Adding a new MIME type here is
// a real security decision — confirm there is no XSS / RCE / decompression-
// bomb risk before extending it. Anything not in this map is rejected.
//
// Values intentionally mirror the table in DOC-865 so the design doc and
// the enforcement code stay in lockstep.
var allowed = func() map[string]MIMEEntry {
	m := map[string]MIMEEntry{}
	add := func(mime string, mode RenderMode, cat Category) {
		m[mime] = MIMEEntry{MIME: mime, RenderMode: mode, Category: cat}
	}

	// --- Images (rendered inline) ---
	for _, t := range []string{"image/png", "image/jpeg", "image/gif", "image/webp",
		"image/avif", "image/heic", "image/heif"} {
		add(t, RenderInline, CategoryImage)
	}

	// --- Video (inline via <video controls>; no transcoding in Phase 1) ---
	for _, t := range []string{"video/mp4", "video/webm", "video/quicktime"} {
		add(t, RenderInline, CategoryVideo)
	}
	// Other video — chip only (browsers won't inline-play these reliably).
	for _, t := range []string{"video/x-matroska", "video/x-msvideo"} {
		add(t, RenderChip, CategoryVideo)
	}

	// --- Audio (inline via <audio controls>) ---
	for _, t := range []string{"audio/mpeg", "audio/wav", "audio/ogg", "audio/webm",
		"audio/flac", "audio/aac", "audio/mp4"} {
		add(t, RenderInline, CategoryAudio)
	}

	// --- Documents (chip with download) ---
	docs := []string{
		"application/pdf",
		"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.ms-excel",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.oasis.opendocument.text",
		"application/vnd.oasis.opendocument.spreadsheet",
		"application/vnd.oasis.opendocument.presentation",
		"application/rtf",
	}
	for _, t := range docs {
		add(t, RenderChip, CategoryDocument)
	}

	// --- Text & data (chip with download) ---
	for _, t := range []string{
		"text/plain", "text/markdown", "text/csv", "text/tab-separated-values",
		"application/json", "application/xml", "text/xml",
		"application/yaml", "text/yaml", "application/toml",
	} {
		add(t, RenderChip, CategoryText)
	}

	// --- Archives ---
	for _, t := range []string{
		"application/zip", "application/x-tar", "application/gzip",
		"application/x-bzip2", "application/x-7z-compressed",
	} {
		add(t, RenderChip, CategoryArchive)
	}

	// --- Forced-download text payloads — would XSS if served inline ---
	for _, t := range []string{
		"text/html", "text/javascript", "application/javascript",
	} {
		add(t, RenderForceDownload, CategoryText)
	}

	return m
}()

// LookupMIME returns the entry for a MIME type if it is on the allowlist,
// or (zero, false) if rejected.
func LookupMIME(mime string) (MIMEEntry, bool) {
	mime = NormalizeMIME(mime)
	e, ok := allowed[mime]
	return e, ok
}

// inlineSafe is the EXPLICIT set of MIME types the read path may serve with
// Content-Disposition: inline (BUG-2413). It is deliberately a standalone
// allowlist and NOT a function of RenderMode. Deriving inline-safety from the
// broad RenderInline bucket would auto-trust every future RenderInline entry, so
// adding an active type (image/svg+xml, say) as RenderInline would silently
// reintroduce same-origin execution. Listing the exact types here means a new
// allowlist entry FAILS SAFE — it downloads until someone makes the explicit,
// reviewable decision to add it here too, the same posture the `allowed` map
// itself takes.
//
// Every member is a format the browser renders WITHOUT executing embedded
// script: raster images, audio and video (all also embedded by the app via
// <img>/<audio>/<video>), plus PDF (sandboxed viewer) and plain text. This is
// the server mirror of the client's VIEWER_MIMES + BROWSER_PREVIEW_MIMES
// (web/src/lib/attachments/display.ts). Notably absent: image/svg+xml and
// application/xhtml+xml (active), text/xml + application/xml (SVG/XHTML wear
// these after an extensionless sniff), and the whole RenderForceDownload bucket.
var inlineSafe = map[string]struct{}{
	// Raster images.
	"image/png": {}, "image/jpeg": {}, "image/gif": {}, "image/webp": {},
	"image/avif": {}, "image/heic": {}, "image/heif": {},
	// Audio (inline via <audio controls>).
	"audio/mpeg": {}, "audio/wav": {}, "audio/ogg": {}, "audio/webm": {},
	"audio/flac": {}, "audio/aac": {}, "audio/mp4": {},
	// Video the app plays inline via <video controls>.
	"video/mp4": {}, "video/webm": {}, "video/quicktime": {},
	// Preview-safe documents.
	"application/pdf": {}, "text/plain": {},
}

// ServeInline reports whether an allowlisted attachment's bytes may be sent with
// Content-Disposition: inline. It is the server-side safety gate for BUG-2413:
// only the explicit passive-media / preview-safe types in `inlineSafe` are
// inline; everything else — the rest of the RenderChip bucket, the whole
// RenderForceDownload bucket, and (at the call site) any MIME not on the
// allowlist at all — is served as an attachment so it cannot execute as
// same-origin active content.
func (e MIMEEntry) ServeInline() bool {
	_, ok := inlineSafe[e.MIME]
	return ok
}

// NormalizeMIME strips parameters and lowercases the type/subtype. We
// match strictly against the allowlist after normalization so callers
// can pass an http.DetectContentType result (which may include a charset
// parameter) without surprises.
func NormalizeMIME(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return strings.ToLower(strings.TrimSpace(mime))
}

// sniffAliases maps stdlib http.DetectContentType quirks to the canonical
// MIME we record in the allowlist (and store in the DB). net/http's
// detector returns names that differ from modern IANA / browser
// conventions for a handful of formats — without aliasing, valid uploads
// get rejected because the allowlist uses canonical names. Aliasing here
// keeps the allowlist single-sourced and predictable.
//
// Add new entries when a Codex round or a real-world upload turns up
// another stdlib mismatch.
var sniffAliases = map[string]string{
	"audio/wave":         "audio/wav",        // .wav
	"application/x-gzip": "application/gzip", // .gz / .tar.gz
}

// SniffMIME detects the MIME type from the leading bytes of a payload
// using net/http's stdlib detector (the same RFC 6838 algorithm browsers
// use). The result is normalized via NormalizeMIME and run through
// sniffAliases so allowlist lookups always see the canonical name.
//
// Pass at most 512 bytes — additional bytes are ignored by the detector.
func SniffMIME(head []byte) string {
	if len(head) > 512 {
		head = head[:512]
	}
	got := NormalizeMIME(http.DetectContentType(head))
	if alias, ok := sniffAliases[got]; ok {
		return alias
	}
	return got
}

// ValidateUpload combines the sniff result, the client-supplied MIME
// header, and the filename's extension to produce one of:
//
//   - (entry, "", nil) — accept; entry.MIME is the canonical type to store
//   - (zero, code, err) — reject; code is a stable machine identifier and
//     err.Error() is a human-readable explanation.
//
// The rule is the conservative version of the DOC-865 spec:
//
//  1. The sniffed type MUST be on the allowlist. Period — we never trust
//     the client's Content-Type header alone.
//  2. The filename's extension, if recognized, must agree with the
//     sniffed type's category (an .exe whose bytes happen to look like a
//     PNG is still rejected, as is a .png that sniffs as text/html).
//
// extOverride lets callers (the multipart handler) pass the original
// filename so we can compare extensions; pass empty string to skip.
func ValidateUpload(head []byte, filename string) (entry MIMEEntry, code string, err error) {
	sniffed := SniffMIME(head)
	e, ok := LookupMIME(sniffed)
	if !ok {
		return MIMEEntry{}, "mime_not_allowed", &uploadError{msg: "MIME type not allowed: " + sniffed}
	}

	// Cross-check filename extension. Two cases:
	//   1. Extension maps to a MIME that itself is blocked (e.g. .svg →
	//      image/svg+xml). This means the file IS the blocked type
	//      regardless of how stdlib sniffed the bytes — http.DetectContentType
	//      classifies SVG as text/xml, which IS on the allowlist, but the
	//      .svg extension makes the browser interpret it as SVG (and run
	//      embedded <script> tags). Reject on extension alone.
	//   2. Extension maps to an allowed MIME but the sniffed category
	//      disagrees (e.g. .pdf with PNG bytes). Reject as a mismatch —
	//      this is the "exe pretending to be png" defense.
	if ext := strings.ToLower(filepath.Ext(filename)); ext != "" {
		if extMIMEStr, hasMapping := extMIMEMap[ext]; hasMapping {
			extEntry, extAllowed := allowed[NormalizeMIME(extMIMEStr)]
			if !extAllowed {
				return MIMEEntry{}, "extension_blocked",
					&uploadError{msg: "Filename extension " + ext + " is not allowed (maps to blocked type " + extMIMEStr + ")"}
			}
			if extEntry.Category != e.Category {
				// Office Open XML documents (.docx / .xlsx / .pptx) are
				// zipped XML containers — http.DetectContentType correctly
				// identifies them as application/zip on the bytes alone.
				// The extension is the only way to distinguish a Word doc
				// from a plain zip, so when the sniff is exactly zip and
				// the extension maps to one of these document MIMEs, we
				// trust the extension. Same logic for OpenDocument
				// formats (.odt/.ods/.odp) which are also zip-based.
				if sniffed == "application/zip" && extEntry.Category == CategoryDocument {
					return extEntry, "", nil
				}
				return MIMEEntry{}, "mime_extension_mismatch",
					&uploadError{msg: "Filename extension " + ext + " does not match detected content type " + sniffed}
			}
		}
	}

	return e, "", nil
}

// uploadError is a small typed error so handlers can keep the error
// surface stable without pulling in a heavyweight error package.
type uploadError struct{ msg string }

func (e *uploadError) Error() string { return e.msg }

// mimeForExt is a minimal extension → entry mapping used only for the
// extension-vs-sniff sanity check. Keeping it small and deliberate means
// unrecognized extensions just skip the cross-check (instead of forcing
// us to enumerate every extension on the planet).
// ExtensionForMIME returns the canonical file extension for a MIME type, or ""
// when there is no mapping.
//
// Exported so a client need not keep its OWN table. The CLI did, and it had
// drifted: it knew images and video but not gzip, tar, XML, YAML, TOML, HTML,
// JavaScript or several document types this map has always allowed — so
// `pad attachment view` silently produced an extensionless file for them,
// defeating its contract of handing a path to something that opens files by
// extension (codex round 26). Two tables for one relationship was the defect.
func ExtensionForMIME(mimeType string) string {
	if ext, ok := canonicalExtForMIME[NormalizeMIME(mimeType)]; ok {
		return ext
	}
	return ""
}

// canonicalExtForMIME reverses extMIMEMap with one deliberate choice per MIME
// type, since several extensions map to the same type. Held to the forward map
// by TestCanonicalExtForMIMECoversTheMap.
var canonicalExtForMIME = buildCanonicalExtForMIME()

// preferredExtensions names the spelling to use where a type has several.
//
// Every key MUST be a value that extMIMEMap actually uses, or the entry is a
// line that cannot fire. One of them was exactly that on first writing —
// "text/yaml", where this map says application/yaml — so the preference never
// applied and .yaml won on length. The test asserts the property rather than
// trusting the next reader to notice.
func preferredExtensions() map[string]string {
	return map[string]string{
		"image/jpeg":       ".jpg",  // over .jpeg
		"text/markdown":    ".md",   // over .markdown
		"application/yaml": ".yml",  // over .yaml
		"text/html":        ".html", // over .htm, which shortest-wins would pick
	}
}

func buildCanonicalExtForMIME() map[string]string {
	preferred := preferredExtensions()
	out := make(map[string]string, len(extMIMEMap))
	for ext, mimeStr := range extMIMEMap {
		m := NormalizeMIME(mimeStr)
		// BLOCKED types get no reverse mapping. extMIMEMap is the forward
		// table used to REFUSE uploads — it deliberately lists .svg, .exe and
		// friends so their extensions can be recognised and rejected.
		// Reversing it wholesale turned that refusal list into a SOURCE of
		// extensions: ExtensionForMIME("image/svg+xml") answered ".svg", and
		// `pad attachment view` names a local file with it. The old CLI table
		// answered nothing for those, so this was a regression, and it
		// reopened the same hazard BUG-2818 describes through a different
		// door (codex round 27).
		//
		// A caller asking "what should I call a file of this type" must only
		// ever be told about types this product actually accepts.
		if _, ok := LookupMIME(m); !ok {
			continue
		}
		if want, ok := preferred[m]; ok {
			out[m] = want
			continue
		}
		// Anything unlisted takes the shortest extension, then alphabetical,
		// so the result does not depend on Go's randomised map order. A helper
		// that answered differently per process would be worse than none.
		cur, seen := out[m]
		if !seen || len(ext) < len(cur) || (len(ext) == len(cur) && ext < cur) {
			out[m] = ext
		}
	}
	return out
}

// SafeFallbackExtension reports whether ext (with its leading dot, any case)
// may be carried into a generic fallback filename when the original name was
// unstorable.
//
// The bar is deliberately higher than "storable text". A fallback name is
// SYNTHESISED by the server, so anything kept from the caller's input has to
// earn its place:
//
//   - it must be a known extension, so an arbitrary suffix cannot ride along
//     and later drive MIME or viewer behaviour that the bytes do not support;
//   - it must map to an ALLOWED type, so the blocklist cannot be sidestepped
//     by arriving through the fallback path instead of the ordinary one;
//
// A control-obfuscated suffix is excluded by the SAME map lookup rather than
// by a separate charset test, and that is a deliberate choice recorded here
// because a mutation exposed it. ".s<VT>vg" is storable (a vertical tab is
// valid UTF-8 and not a NUL) but matches no key, so it is already refused.
// I first wrote an explicit alphanumeric loop as well; removing it changed
// nothing, because no key in extMIMEMap contains a non-alphanumeric character.
// Keeping a guard that cannot fire, with a comment claiming it stops control
// characters, would have misdescribed which line does the work — so the loop
// is gone and TestExtMIMEMapKeysArePlain enforces the property it relied on.
//
// That divergence — storable here, stripped by Content-Disposition sanitising,
// so ".s<VT>vg" reaches the client as ".svg" past a blocklist that never
// evaluated it — is a pre-existing hazard on the ORDINARY upload path, where
// the name is not synthesised at all. Tracked as BUG-2818; this predicate only
// refuses to add a second door to it.
//
// Anything else is dropped and the fallback stays extensionless.
func SafeFallbackExtension(ext string) bool {
	if len(ext) < 2 || len(ext) > 16 || ext[0] != '.' {
		return false
	}
	mimeStr, ok := extMIMEMap[strings.ToLower(ext)]
	if !ok {
		return false
	}
	_, allowed := LookupMIME(NormalizeMIME(mimeStr))
	return allowed
}

var extMIMEMap = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".heic": "image/heic",
	".heif": "image/heif",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".avi":  "video/x-msvideo",
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".flac": "audio/flac",
	".aac":  "audio/aac",
	".m4a":  "audio/mp4",
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".odp":  "application/vnd.oasis.opendocument.presentation",
	".rtf":  "application/rtf",
	".txt":  "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
	".tsv":  "text/tab-separated-values",
	".json": "application/json",
	".xml":  "application/xml",
	".yaml": "application/yaml",
	".yml":  "application/yaml",
	".toml": "application/toml",
	".zip":  "application/zip",
	".tar":  "application/x-tar",
	".gz":   "application/gzip",
	".bz2":  "application/x-bzip2",
	".7z":   "application/x-7z-compressed",
	".html": "text/html",
	".htm":  "text/html",
	".js":   "text/javascript",

	// Known-blocked: included here ONLY so ValidateUpload can see them
	// and reject by extension. None of these are on the `allowed` map.
	".svg": "image/svg+xml",
	".exe": "application/x-msdownload",
	".dll": "application/x-msdownload",
	".msi": "application/x-msi",
	".bat": "application/x-bat",
	".sh":  "application/x-sh",
	".com": "application/x-msdownload",
	".dmg": "application/x-apple-diskimage",
	".deb": "application/vnd.debian.binary-package",
	".rpm": "application/x-rpm",
	".app": "application/octet-stream",
}
