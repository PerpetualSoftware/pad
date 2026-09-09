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
	// embedding the bytes (PDFs, archives, office docs). Content-Disposition
	// for these is decided by the read path's explicit ServeInline allowlist
	// (BUG-2413), NOT by this mode: most chip types are served as
	// "attachment", with only the audited exceptions (PDF, say) inline. An
	// earlier version of this comment said the HTTP layer served every chip
	// inline, which described the pre-BUG-2413 policy (codex closing round 8).
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
		"application/json", "text/xml",
		"application/yaml", "application/toml",
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
	// application/javascript was removed here (BUG-2963 F6): no extension in
	// extMIMEMap reaches that spelling and SniffMIME cannot emit it, so the
	// entry could never be the type an upload was stored under. text/javascript
	// stays because .js maps to it — and since F5 it is REACHABLE: a .js upload
	// sniffs text/plain and the extension chooses this spelling, which moves
	// the file out of the inline-safe text/plain entry and into forced
	// download. That is the direction of the trade and the reason .js was
	// included rather than carved out. (This comment said "not reachable
	// EITHER" until F5 made it false.)
	for _, t := range []string{
		"text/html", "text/javascript",
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
// application/xhtml+xml (active), text/xml (SVG and XHTML wear it after an
// extensionless sniff), and the whole RenderForceDownload bucket. The
// application/xml spelling used to be named here alongside text/xml; it left
// the allowlist entirely in BUG-2963 F6, so there is no entry left to exclude.
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
	"video/avi":          "video/x-msvideo",  // .avi — pure spelling difference
	// application/ogg is deliberately NOT here, and there is no per-codec
	// refinement either — both were written and both were removed. An alias
	// table is for two names of ONE thing, and Ogg is one name for several:
	// see mime_magic.go for why a container name cannot be resolved to an
	// audio type from the head of the file.
}

// SniffMIME detects the MIME type from the leading bytes of a payload
// using net/http's stdlib detector (the same RFC 6838 algorithm browsers
// use). The result is normalized via NormalizeMIME and run through
// sniffAliases so allowlist lookups always see the canonical name.
//
// Beyond the ISO-BMFF pre-check below, three refinements run AFTER the stdlib,
// each keyed on what it said: the WebM DocType read, the magic table for bytes
// it had no opinion about, and RTF's five-byte signature. They are described
// at the switch that dispatches them.
//
// One family is detected ahead of the stdlib: ISO-BMFF. Still images
// (HEIC / HEIF / AVIF) are the original case — the mimesniff table has no
// signature for them, so they sniffed as application/octet-stream and were
// unreachable behind an allowlist that names all three (BUG-2961). BUG-2963 F4
// added two audio/video MAJOR brands: "qt  " (QuickTime), which the stdlib
// also has no signature for, and "M4A ", which it names video/mp4 from a
// compatible mp41 brand — the one place this package overrides the stdlib,
// argued at isoBMFFAVBrands. sniffISOBMFF returns "" for everything else, so
// apart from that one brand it can only add detections; see mime_isobmff.go.
//
// Pass at most 512 bytes — additional bytes are ignored by the detector.
func SniffMIME(head []byte) string {
	if len(head) > 512 {
		head = head[:512]
	}
	if mime := sniffISOBMFF(head); mime != "" {
		return mime
	}
	got := NormalizeMIME(http.DetectContentType(head))
	if alias, ok := sniffAliases[got]; ok {
		got = alias
	}
	// Three BUG-2963 refinements. (Ogg was a fourth and was removed; see
	// mime_magic.go for why a container name cannot be aliased to an audio
	// type.) Each is keyed on what the stdlib already said, so none can retype
	// a file the standard library identified. None VALIDATES the format — see
	// mime_magic.go's header for the three review rounds that settled why
	// recognition here is by magic:
	//
	//   - video/webm is refined, because the mimesniff table answers it from
	//     the bare EBML magic and cannot tell Matroska from WebM;
	//   - application/octet-stream is the stdlib having NO opinion, which is
	//     the case where recognising more formats adds the most;
	//   - text/plain is an OPINION, so it admits exactly one signature, RTF's
	//     five fixed bytes. The bar for adding a second is the argument at
	//     validRTFStream, not this list's existence.
	switch got {
	case "video/webm":
		if mime := sniffEBMLDocType(head); mime != "" {
			return mime
		}
	case "application/octet-stream":
		if mime := sniffOpaqueMagic(head); mime != "" {
			return mime
		}
	case "text/plain":
		// RTF is printable ASCII, so text/plain is an OPINION here rather
		// than the absence of one — which is why this case is narrower than
		// the octet-stream case above and admits exactly one signature. Five
		// fixed bytes at offset zero; nothing else in this switch may key on
		// text/plain without the same argument (BUG-2963).
		if validRTFStream(head) {
			return "application/rtf"
		}
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
	stdlib := NormalizeMIME(http.DetectContentType(head))
	if alias, ok := sniffAliases[stdlib]; ok {
		stdlib = alias
	}
	sniffed := SniffMIME(head)
	ext := strings.ToLower(filepath.Ext(filename))

	// More than one magic can match the same bytes — a tar whose first member
	// is named "fLaC.txt", a FLAC whose COMMENT tag contains "ustar". When the
	// extension names one of the matching candidates, it breaks the tie. It
	// cannot introduce a type: every candidate is one the bytes matched, and a
	// name for a type whose magic is absent never appears in the list.
	if stdlib == "application/octet-stream" {
		if alt := preferCandidateForExt(sniffOpaqueCandidates(head), ext); alt != "" {
			sniffed = alt
		}
	}

	// Raw AAC is the one BUG-2963 format whose structure is too small to act
	// on from the bytes alone, so it is resolved here — where the filename is
	// known — rather than inside SniffMIME, which deliberately never sees a
	// filename. Both halves are required, and neither is sufficient: bytes
	// without the extension name nothing, and the extension without a valid
	// ADTS header names nothing either. The extension does not supply
	// evidence; it decides whether a weak structure may speak.
	//
	// What it does NOT do is decide the outcome for a .aac generally. A .aac
	// whose bytes are some other allowlisted audio type is still stored as
	// that type by the ordinary rules — the categories agree, so nothing here
	// refuses it. This branch adds one reading; it removes none.
	// The gate is on THE STANDARD LIBRARY'S verdict, not on the refined one,
	// and the difference is not academic: a real AAC frame whose ancillary
	// payload contains "ustar" at offset 257 is refined to application/x-tar
	// by this package, and gating on the refined value refused it under its
	// own .aac name. The stdlib said octet-stream about that file — nothing
	// identified it — which is the condition under which a weak signature may
	// speak.
	//
	// Both no-opinion verdicts count. text/plain is the second: an AAC frame
	// whose ancillary payload is printable makes the leading bytes look
	// textual, and leaving that verdict out refused real files too.
	//
	// A file the stdlib DOES identify is untouched. That the gate can fire at
	// all is a property worth keeping rather than a formality: no signature in
	// the mimesniff table begins with 0xFF today, so nothing the stdlib names
	// can pass validADTSHeader — but if one ever does, this gate is what stops
	// a fourteen-bit match from overriding it.
	if (stdlib == "application/octet-stream" || stdlib == "text/plain") &&
		validADTSHeader(head) && ext == ".aac" {
		sniffed = "audio/aac"
	}

	// The legacy Office trio (BUG-2963). CFB is a container the stdlib has no
	// signature for, so .doc/.xls/.ppt sniffed application/octet-stream and
	// were refused while all three types sat on the allowlist. This is the
	// zip+document branch's shape one container family over: the BYTES say
	// "CFB container" and nothing finer, and the extension chooses which of
	// the three reviewed Office types is stored.
	//
	// The extension set is written out here rather than taken from extMIMEMap,
	// because the question is not "does this extension map to a document" —
	// .msi is a CFB container too, and so are Visio files. It is "is this one
	// of the three types the allowlist reviewed", and that is a list, not a
	// predicate. Everything else keeps falling through to mime_not_allowed.
	if sniffed == "application/octet-stream" && validCFBHeader(head) {
		switch ext {
		case ".doc", ".xls", ".ppt":
			if extEntry, extAllowed := allowed[NormalizeMIME(extMIMEMap[ext])]; extAllowed &&
				extEntry.Category == CategoryDocument {
				sniffed = extEntry.MIME
			}
		}
	}

	// The text family (BUG-2963 F5). text/plain is the stdlib saying "these
	// bytes are text" and nothing more — it has no signature that separates
	// Markdown from YAML from JavaScript, because at the byte level there is
	// none to have. So the extension chooses WHICH text, and that is the whole
	// of what it does: the bytes established the category, the filename
	// chooses the spelling inside it, and the mapped entry must itself be an
	// allowlisted TEXT entry or this does not fire.
	//
	// Category-preserving is what makes this the smallest trust of the three
	// in PR B. Nothing crosses a category boundary, so no file becomes an
	// image, an archive or a document by being renamed. What CAN change is the
	// render mode, and only in the safe direction: .js and .html map to
	// entries in the RenderForceDownload bucket, so a file that used to be
	// stored as text/plain and offered as a chip is now marked
	// Content-Disposition: attachment. More conservative than what it
	// replaces, which is why those two are in rather than carved out.
	//
	// An extension whose mapping is NOT on the allowlist falls through
	// untouched — .svg is the case that matters, and it must keep reaching the
	// extension_blocked rule below rather than being quietly stored as text.
	if sniffed == "text/plain" && ext != "" {
		if extMIMEStr, hasMapping := extMIMEMap[ext]; hasMapping {
			if extEntry, extAllowed := allowed[NormalizeMIME(extMIMEStr)]; extAllowed &&
				extEntry.Category == CategoryText {
				sniffed = extEntry.MIME
			}
		}
	}

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
				// The audio/video split inside the MP4 family (BUG-2963 F4).
				// Audio-only and video MP4 files are the same container, so
				// the stdlib answers video/mp4 for both and an .m4a is
				// refused as a mismatch against its own type. This is the
				// zip+document trust one family over: the BYTES establish the
				// container (ISO-BMFF, mp4-branded — nothing else reaches
				// video/mp4 here), and the filename chooses only which
				// spelling WITHIN that family is stored. It is not a track
				// read and does not pretend to be. A video file renamed .m4a
				// is stored audio/mp4, which costs an audio player where a
				// video player belonged and nothing else: both are on the
				// allowlist, both render inline, nothing is executed.
				//
				// The "M4A " major brand needs none of this — sniffISOBMFF
				// answers audio/mp4 from the bytes. This is the isom-branded
				// case, which is what FFmpeg writes and what arrives from
				// phones.
				// The zip branch above guards on extEntry.Category because it
				// answers for many extensions at once. This one answers for
				// exactly .m4a, so the same guard could never differ from the
				// extension test beside it — dead by construction rather than
				// defence in depth, and a mutation run says so: removing it
				// survives. What the branch actually depends on is that .m4a
				// maps to an allowlisted AUDIO entry, which is a property of
				// extMIMEMap and is asserted as one by
				// TestBUG2963F4M4AExtensionTrust. A remap fails that test
				// rather than silently retyping MP4 bytes here.
				if sniffed == "video/mp4" && ext == ".m4a" {
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
// "text/yaml", where the forward map says application/yaml — so the preference
// never applied and .yaml won on length. (text/yaml is no longer even on the
// allowlist; BUG-2963 F6 removed it as unreachable. The lesson it taught this
// map is why the note survives it.) The test asserts the property rather than
// trusting the next reader to notice.
func preferredExtensions() map[string]string {
	return map[string]string{
		"image/jpeg":       ".jpg",  // over .jpeg
		"application/yaml": ".yml",  // over .yaml
		"text/html":        ".html", // over .htm, which shortest-wins would pick
		// text/markdown is NOT here: .md is its only extMIMEMap spelling, so
		// a preference had nothing to choose against — a line that cannot
		// fire, the same class this map's doc warns about (codex closing
		// round 5). The test now asserts every entry has a real competitor.
	}
}

// aliasExtensions covers allowed MIME spellings that NO extension in
// extMIMEMap maps to, so reversing the forward table alone leaves them
// without an extension: the forward table picks one spelling per extension
// (.webm says video/webm), while the allowlist accepts the alias spellings
// too. An attachment stored under an alias type with an unstorable filename
// came out of `pad attachment view` extensionless — the exact failure the
// delegation to this package was built to end (codex closing round).
//
// This table held four entries until BUG-2963 F6. Three stopped being
// alias-shaped for two different reasons, and the distinction is the thing to
// keep: text/yaml and application/javascript were REMOVED from the allowlist
// as unreachable spellings, so an alias for them would name a refused type;
// text/xml is still very much allowed, but the forward map now spells .xml
// with it, so the reverse mapping is derived and an alias entry here would be
// a duplicate the hygiene assertion below rejects. An entry leaving this
// table therefore says nothing on its own about whether the type survived.
//
// Every key MUST be an allowed type with no forward-derived reverse mapping,
// and every value MUST be an extension the forward map sends to an ALLOWED
// type — an alias must never mint an extension the product refuses (the
// BUG-2818 door the blocked-type filter above closes). Both properties are
// asserted by test, alongside the closing property this table exists for:
// every allowed MIME type has a reverse extension.
func aliasExtensions() map[string]string {
	return map[string]string{
		"audio/webm": ".webm", // forward map spells it video/webm; the container is the same
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
	// Alias spellings last. Unconditional on purpose: an alias key that the
	// forward loop ALSO produced would mean two sources disagree about one
	// type, and the test asserting alias keys have no forward-derived mapping
	// fails loudly rather than letting this line silently pick a winner.
	for m, ext := range aliasExtensions() {
		out[NormalizeMIME(m)] = ext
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
	// text/xml, not application/xml: the latter left the allowlist in
	// BUG-2963 F6, and this map's values are looked up in `allowed` — an
	// extension pointing at a removed spelling would make every .xml upload
	// fail extension_blocked, which is the same map's mechanism for refusing
	// .svg and .exe.
	".xml":  "text/xml",
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
