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

// SniffMIME detects the MIME type from the leading bytes of a payload
// using net/http's stdlib detector (the same RFC 6838 algorithm browsers
// use). The result is normalized via NormalizeMIME.
//
// Pass at most 512 bytes — additional bytes are ignored by the detector.
func SniffMIME(head []byte) string {
	if len(head) > 512 {
		head = head[:512]
	}
	return NormalizeMIME(http.DetectContentType(head))
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
			extEntry, allowed := allowed[NormalizeMIME(extMIMEStr)]
			if !allowed {
				return MIMEEntry{}, "extension_blocked",
					&uploadError{msg: "Filename extension " + ext + " is not allowed (maps to blocked type " + extMIMEStr + ")"}
			}
			if extEntry.Category != e.Category {
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
	".svg":  "image/svg+xml",
	".exe":  "application/x-msdownload",
	".dll":  "application/x-msdownload",
	".msi":  "application/x-msi",
	".bat":  "application/x-bat",
	".sh":   "application/x-sh",
	".com":  "application/x-msdownload",
	".dmg":  "application/x-apple-diskimage",
	".deb":  "application/vnd.debian.binary-package",
	".rpm":  "application/x-rpm",
	".app":  "application/octet-stream",
}

