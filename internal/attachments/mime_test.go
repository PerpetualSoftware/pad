package attachments

import (
	"strings"
	"testing"
)

func TestNormalizeMIME(t *testing.T) {
	cases := map[string]string{
		"image/png":                 "image/png",
		"IMAGE/PNG":                 "image/png",
		"text/plain; charset=utf-8": "text/plain",
		"  image/png  ":             "image/png",
	}
	for in, want := range cases {
		if got := NormalizeMIME(in); got != want {
			t.Errorf("NormalizeMIME(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLookupMIME_AllowedAndRejected(t *testing.T) {
	allowedSamples := []string{
		"image/png", "image/jpeg", "image/webp",
		"video/mp4", "audio/mpeg",
		"application/pdf", "text/plain", "application/zip",
		"text/html", // forced-download
	}
	for _, m := range allowedSamples {
		if _, ok := LookupMIME(m); !ok {
			t.Errorf("LookupMIME(%q) = !ok, want allowed", m)
		}
	}

	rejected := []string{
		"image/svg+xml",                                 // explicit XSS-vector block
		"application/x-msdownload",                      // executable
		"application/x-executable",                      // executable
		"application/vnd.microsoft.portable-executable", // executable
		"application/octet-stream",                      // unknown
		"",                                              // empty
	}
	for _, m := range rejected {
		if _, ok := LookupMIME(m); ok {
			t.Errorf("LookupMIME(%q) = ok, want rejected", m)
		}
	}
}

func TestLookupMIME_RenderModes(t *testing.T) {
	must := func(m string) MIMEEntry {
		t.Helper()
		e, ok := LookupMIME(m)
		if !ok {
			t.Fatalf("LookupMIME(%q) rejected", m)
		}
		return e
	}
	if must("image/png").RenderMode != RenderInline {
		t.Errorf("image/png should render inline")
	}
	if must("application/pdf").RenderMode != RenderChip {
		t.Errorf("application/pdf should render as chip")
	}
	if must("text/html").RenderMode != RenderForceDownload {
		t.Errorf("text/html must force download")
	}
}

// BUG-2413: ServeInline is the read path's inline-safe gate. Passive media plus
// PDF and plain text may be served inline; every other allowlisted type — the
// rest of the RenderChip bucket and the whole force-download bucket — must not.
func TestServeInline(t *testing.T) {
	must := func(m string) MIMEEntry {
		t.Helper()
		e, ok := LookupMIME(m)
		if !ok {
			t.Fatalf("LookupMIME(%q) rejected", m)
		}
		return e
	}
	inline := []string{
		"image/png", "image/jpeg", "image/gif", "image/webp", "image/avif",
		"audio/mpeg", "video/mp4", // passive media the app embeds inline
		"application/pdf", "text/plain", // the preview-safe chip subset
	}
	for _, m := range inline {
		if !must(m).ServeInline() {
			t.Errorf("ServeInline(%q) = false, want true (inline-safe)", m)
		}
	}
	download := []string{
		"text/xml", "application/xml", "application/json", "text/csv",
		"text/markdown", "application/msword", "application/zip",
		"text/html", "text/javascript", "application/javascript",
	}
	for _, m := range download {
		if must(m).ServeInline() {
			t.Errorf("ServeInline(%q) = true, want false (must download)", m)
		}
	}
}

// minimal PNG header
var pngHeader = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}

// minimal JPEG header (SOI marker)
var jpegHeader = []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0, 0, 0, 0x4a, 0x46, 0x49, 0x46}

// PE/EXE header
var exeHeader = []byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xff\x00\x00")

func TestSniffMIME(t *testing.T) {
	cases := map[string][]byte{
		"image/png":                pngHeader,
		"image/jpeg":               jpegHeader,
		"text/plain":               []byte("hello world\nthis is plain text\n"),
		"application/octet-stream": exeHeader, // sniff doesn't classify EXE specifically
	}
	for want, head := range cases {
		got := SniffMIME(head)
		// stdlib sniff sometimes returns "text/plain; charset=utf-8" — NormalizeMIME handles that.
		if got != want && !strings.HasPrefix(got, want+";") {
			t.Errorf("SniffMIME(%v...) = %q, want %q", head[:4], got, want)
		}
	}
}

func TestValidateUpload_HappyPath(t *testing.T) {
	entry, code, err := ValidateUpload(pngHeader, "screenshot.png")
	if err != nil {
		t.Fatalf("err = %v code=%s", err, code)
	}
	if entry.MIME != "image/png" {
		t.Errorf("entry.MIME = %q", entry.MIME)
	}
	if entry.Category != CategoryImage {
		t.Errorf("entry.Category = %q", entry.Category)
	}
}

func TestValidateUpload_RejectsExe(t *testing.T) {
	_, code, err := ValidateUpload(exeHeader, "totally-safe.png")
	if err == nil {
		t.Fatal("expected rejection")
	}
	// Sniff yields application/octet-stream, which is not on the allowlist.
	if code != "mime_not_allowed" {
		t.Errorf("code = %q, want mime_not_allowed", code)
	}
}

func TestValidateUpload_RejectsExtensionMismatch(t *testing.T) {
	// PNG bytes but .pdf extension → category mismatch (image vs document)
	_, code, err := ValidateUpload(pngHeader, "evil.pdf")
	if err == nil {
		t.Fatal("expected rejection")
	}
	if code != "mime_extension_mismatch" {
		t.Errorf("code = %q, want mime_extension_mismatch", code)
	}
}

func TestValidateUpload_RejectsSVG(t *testing.T) {
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	_, code, err := ValidateUpload(svg, "logo.svg")
	if err == nil {
		t.Fatal("expected rejection — SVG is on the explicit blocklist")
	}
	// SVG sniffs as text/xml in stdlib (which IS on the allowlist) so the
	// extension blocklist must do the work. The .svg extension maps to
	// image/svg+xml which is NOT on the `allowed` map → extension_blocked.
	if code != "mime_not_allowed" && code != "mime_extension_mismatch" && code != "extension_blocked" {
		t.Errorf("code = %q, want one of mime_not_allowed/mime_extension_mismatch/extension_blocked", code)
	}
}

func TestValidateUpload_AllowsTextPlain(t *testing.T) {
	body := []byte("just plain text\n")
	entry, code, err := ValidateUpload(body, "notes.txt")
	if err != nil {
		t.Fatalf("err=%v code=%s", err, code)
	}
	if entry.RenderMode != RenderChip {
		t.Errorf("text/plain should render as chip")
	}
}

func TestValidateUpload_HTMLForcedDownload(t *testing.T) {
	body := []byte("<!doctype html><html><body><script>alert(1)</script></body></html>")
	entry, code, err := ValidateUpload(body, "page.html")
	if err != nil {
		t.Fatalf("err=%v code=%s", err, code)
	}
	if entry.RenderMode != RenderForceDownload {
		t.Errorf("html must be force-download, got %v", entry.RenderMode)
	}
}

// Minimal zip header: PK\x03\x04 + version + flags + method + ...
// This is what application/zip sniffs to. Office Open XML documents
// share this header because they are zipped XML containers.
var zipHeader = []byte{
	0x50, 0x4b, 0x03, 0x04, 0x14, 0x00, 0x00, 0x00,
	0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

func TestValidateUpload_AcceptsOfficeOpenXMLAsZipBytes(t *testing.T) {
	// .docx / .xlsx / .pptx are zipped XML — http.DetectContentType
	// correctly returns application/zip for the bytes. The extension is
	// the only signal that distinguishes a Word doc from a plain zip,
	// so the validator must trust the extension in this case.
	cases := map[string]string{
		"report.docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"sheet.xlsx":  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"deck.pptx":   "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"notes.odt":   "application/vnd.oasis.opendocument.text",
		"calc.ods":    "application/vnd.oasis.opendocument.spreadsheet",
		"slides.odp":  "application/vnd.oasis.opendocument.presentation",
	}
	for filename, wantMIME := range cases {
		entry, code, err := ValidateUpload(zipHeader, filename)
		if err != nil {
			t.Errorf("ValidateUpload(%q) rejected (code=%s): %v", filename, code, err)
			continue
		}
		if entry.MIME != wantMIME {
			t.Errorf("ValidateUpload(%q) entry.MIME = %q, want %q", filename, entry.MIME, wantMIME)
		}
		if entry.Category != CategoryDocument {
			t.Errorf("ValidateUpload(%q) entry.Category = %q, want document", filename, entry.Category)
		}
	}

	// Plain .zip with the same bytes still routes to application/zip / archive.
	entry, code, err := ValidateUpload(zipHeader, "stuff.zip")
	if err != nil {
		t.Fatalf("plain .zip rejected (code=%s): %v", code, err)
	}
	if entry.MIME != "application/zip" || entry.Category != CategoryArchive {
		t.Errorf("plain .zip routed wrong: mime=%q cat=%q", entry.MIME, entry.Category)
	}
}

// TestSniffMIME_AliasesStdlibQuirks regression-tests Codex round 3:
// http.DetectContentType returns names that don't match modern IANA
// conventions for WAV (audio/wave) and gzip (application/x-gzip).
// SniffMIME must alias these to the canonical names so allowlist
// lookups succeed.
func TestSniffMIME_AliasesStdlibQuirks(t *testing.T) {
	// Real WAV header: "RIFF....WAVEfmt "
	wav := []byte{
		'R', 'I', 'F', 'F', 0x24, 0, 0, 0, 'W', 'A', 'V', 'E',
		'f', 'm', 't', ' ', 0x10, 0, 0, 0, 0x01, 0, 0x01, 0,
		0x44, 0xAC, 0, 0, 0x88, 0x58, 0x01, 0,
	}
	if got := SniffMIME(wav); got != "audio/wav" {
		t.Errorf("SniffMIME(WAV) = %q, want audio/wav (stdlib returns audio/wave; alias must canonicalize)", got)
	}

	// Gzip magic: 0x1f 0x8b 0x08
	gzip := []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0, 0, 0xff, 'h', 'i', '\n'}
	if got := SniffMIME(gzip); got != "application/gzip" {
		t.Errorf("SniffMIME(gzip) = %q, want application/gzip (stdlib returns application/x-gzip; alias must canonicalize)", got)
	}
}

func TestValidateUpload_AcceptsWAV(t *testing.T) {
	wav := []byte{
		'R', 'I', 'F', 'F', 0x24, 0, 0, 0, 'W', 'A', 'V', 'E',
		'f', 'm', 't', ' ', 0x10, 0, 0, 0, 0x01, 0, 0x01, 0,
		0x44, 0xAC, 0, 0, 0x88, 0x58, 0x01, 0,
	}
	entry, code, err := ValidateUpload(wav, "song.wav")
	if err != nil {
		t.Fatalf("ValidateUpload(.wav) rejected (code=%s): %v", code, err)
	}
	if entry.MIME != "audio/wav" || entry.Category != CategoryAudio {
		t.Errorf("entry = {%s, %s}, want {audio/wav, audio}", entry.MIME, entry.Category)
	}
}

func TestValidateUpload_AcceptsGzip(t *testing.T) {
	gzip := []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0, 0, 0xff, 'h', 'i', '\n'}
	entry, code, err := ValidateUpload(gzip, "logs.gz")
	if err != nil {
		t.Fatalf("ValidateUpload(.gz) rejected (code=%s): %v", code, err)
	}
	if entry.MIME != "application/gzip" || entry.Category != CategoryArchive {
		t.Errorf("entry = {%s, %s}, want {application/gzip, archive}", entry.MIME, entry.Category)
	}
}

func TestValidateUpload_RejectsExeByExtensionAlone(t *testing.T) {
	// Random bytes (no executable magic) named like an executable. The
	// extension-blocklist must reject regardless of sniff so an attacker
	// can't smuggle an EXE inside what sniffs as text/plain.
	body := []byte("just text masquerading as a binary\n")
	_, code, err := ValidateUpload(body, "payload.exe")
	if err == nil {
		t.Fatal("expected rejection on .exe extension alone")
	}
	if code != "extension_blocked" {
		t.Errorf("code = %q, want extension_blocked", code)
	}
}

// TestExtMIMEMapKeysArePlain enforces the property SafeFallbackExtension
// relies on instead of restating it as a comment.
//
// That predicate has no charset test of its own: a control-obfuscated suffix
// like ".s<VT>vg" is refused because it matches no key here, not because
// anything inspects its bytes. I originally wrote an explicit alphanumeric
// loop too, and removing it changed no behaviour — a guard that cannot fire,
// carrying a comment about what it protects against, misdescribes which line
// does the work.
//
// So the loop is gone and this test holds the invariant up. If a key with a
// dash, a space, or a control byte is ever added, this fails and whoever adds
// it has to decide whether SafeFallbackExtension needs its own charset test
// back.
func TestExtMIMEMapKeysArePlain(t *testing.T) {
	if len(extMIMEMap) == 0 {
		t.Fatal("premise failed: the extension map is empty, so this test asserts nothing")
	}
	for ext := range extMIMEMap {
		if len(ext) < 2 || ext[0] != '.' {
			t.Errorf("extension key %q must start with a dot and have a body", ext)
			continue
		}
		for _, r := range ext[1:] {
			if (r < '0' || r > '9') && (r < 'a' || r > 'z') {
				t.Errorf("extension key %q contains %q, which is not lowercase alphanumeric; "+
					"SafeFallbackExtension has no charset test of its own and relies on this", ext, r)
			}
		}
	}
}

// TestCanonicalExtForMIMECoversTheMap holds the reverse map to the forward one
// so the two cannot drift.
//
// The CLI used to keep its own MIME-to-extension table, which had fallen
// behind this package's — it knew images and video but not gzip, tar, XML,
// YAML, TOML, HTML, JavaScript or several documents, so `pad attachment view`
// silently produced extensionless files for them. The CLI now delegates here,
// which removes the second table; this test stops the reverse map growing the
// same gap against the forward one.
func TestCanonicalExtForMIMECoversTheMap(t *testing.T) {
	if len(extMIMEMap) == 0 {
		t.Fatal("premise failed: the forward map is empty, so this asserts nothing")
	}
	var sawBlocked bool
	for ext, mimeStr := range extMIMEMap {
		m := NormalizeMIME(mimeStr)
		got := ExtensionForMIME(m)

		// A BLOCKED type must have no reverse mapping. extMIMEMap is the
		// refusal table — it lists .svg and .exe so uploads carrying them can
		// be rejected — and reversing it wholesale turned a refusal list into
		// a source of extensions that `pad attachment view` then names local
		// files with (codex round 27).
		if _, allowed := LookupMIME(m); !allowed {
			sawBlocked = true
			if got != "" {
				t.Errorf("%s is BLOCKED but ExtensionForMIME returns %q; a client would name a local "+
					"file with an extension this product refuses to accept", m, got)
			}
			continue
		}

		if got == "" {
			t.Errorf("%s (from %q) has no reverse mapping; a client asking for its extension gets "+
				"nothing and writes an extensionless file", m, ext)
			continue
		}
		if back, ok := extMIMEMap[got]; !ok || NormalizeMIME(back) != m {
			t.Errorf("ExtensionForMIME(%q) = %q, which does not map back to it", m, got)
		}
	}

	// Premise: the map must actually CONTAIN a blocked type, or the blocked
	// branch above asserts nothing and would pass against a reverse map that
	// happily emitted .svg.
	if !sawBlocked {
		t.Fatal("premise failed: no blocked MIME type found in extMIMEMap, so the blocked-type " +
			"assertion never ran")
	}
	if got := ExtensionForMIME("image/svg+xml"); got != "" {
		t.Errorf("image/svg+xml is the named reason the extension blocklist exists; "+
			"ExtensionForMIME returned %q", got)
	}

	// Every preferred spelling must name a MIME type this map actually uses.
	// A preference for a type that is not here is a line that cannot fire,
	// and one of them was: "text/yaml", where the map says application/yaml.
	values := map[string]bool{}
	for _, m := range extMIMEMap {
		values[NormalizeMIME(m)] = true
	}
	for m, ext := range preferredExtensions() {
		if !values[m] {
			t.Errorf("preferred extension %q is declared for %q, which no entry in extMIMEMap uses; "+
				"the preference can never apply", ext, m)
		}
		if back, ok := extMIMEMap[ext]; !ok || NormalizeMIME(back) != m {
			t.Errorf("preferred extension %q for %q does not map back to it", ext, m)
		}
	}

	// Stability: the reverse map is built by iterating a Go map, whose order
	// is randomised. A helper that answered differently per process would be
	// worse than none, so the choice must be deterministic — for EVERY entry,
	// not only the preferred ones. The first version of this loop compared
	// just the preference list, so a non-preferred mapping could have
	// regressed to map-order selection while it stayed green (codex closing
	// round 2). Now every rebuild must equal the first, and the preferences
	// are additionally pinned to their declared spellings.
	first := buildCanonicalExtForMIME()
	for m, want := range preferredExtensions() {
		if got := first[m]; got != want {
			t.Fatalf("wrong reverse mapping for %s: got %q want %q", m, got, want)
		}
	}
	for i := 0; i < 20; i++ {
		built := buildCanonicalExtForMIME()
		if len(built) != len(first) {
			t.Fatalf("reverse map size changed across rebuilds: %d vs %d on iteration %d",
				len(built), len(first), i)
		}
		for m, want := range first {
			if got := built[m]; got != want {
				t.Fatalf("unstable reverse mapping for %s: got %q want %q on iteration %d",
					m, got, want, i)
			}
		}
	}
}

// TestEveryAllowedMIMEHasAnExtension is the closing property the alias table
// exists for: reversing extMIMEMap alone left four ALLOWED spellings
// (text/xml, text/yaml, application/javascript, audio/webm) with no reverse
// extension, because the forward map picks one spelling per extension and the
// allowlist accepts more spellings than the forward map uses. The population
// is asserted over the allowlist itself, not the four known cases, so a
// future allowlist entry without a reverse extension fails here instead of
// shipping another extensionless `pad attachment view` file (codex closing
// round, BUG-2803).
func TestEveryAllowedMIMEHasAnExtension(t *testing.T) {
	if len(allowed) == 0 {
		t.Fatal("premise failed: the allowlist is empty, so this asserts nothing")
	}
	for m := range allowed {
		got := ExtensionForMIME(m)
		if got == "" {
			t.Errorf("%s is ALLOWED but has no reverse extension; `pad attachment view` writes an "+
				"extensionless file for it", m)
			continue
		}
		// Whatever extension is handed out, the forward map must know it and
		// send it to an ALLOWED type — the reverse map must never mint an
		// extension the product refuses (the BUG-2818 door).
		back, ok := extMIMEMap[got]
		if !ok {
			t.Errorf("ExtensionForMIME(%q) = %q, an extension extMIMEMap does not know", m, got)
			continue
		}
		if _, backAllowed := LookupMIME(back); !backAllowed {
			t.Errorf("ExtensionForMIME(%q) = %q, which the forward map sends to BLOCKED type %q", m, got, back)
		}
	}

	// Alias-table hygiene: each key must be an allowed type the forward loop
	// produces NO mapping for (the alias application is unconditional, so a
	// forward-derived key here would be silently overridden — this assertion
	// is what makes that loud instead).
	values := map[string]bool{}
	for _, mv := range extMIMEMap {
		values[NormalizeMIME(mv)] = true
	}
	for m, ext := range aliasExtensions() {
		nm := NormalizeMIME(m)
		if nm != m {
			t.Errorf("alias key %q is not in normalized form (want %q)", m, nm)
		}
		if _, ok := LookupMIME(nm); !ok {
			t.Errorf("alias %q is not on the allowlist; an alias for a refused type is a line that "+
				"cannot legitimately fire", m)
		}
		if values[nm] {
			t.Errorf("alias %q is a value extMIMEMap already uses — the forward loop derives its "+
				"mapping and the alias would override it", m)
		}
		if back, ok := extMIMEMap[ext]; !ok {
			t.Errorf("alias extension %q for %q is unknown to extMIMEMap", ext, m)
		} else if _, backAllowed := LookupMIME(back); !backAllowed {
			t.Errorf("alias extension %q for %q maps to BLOCKED type %q", ext, m, back)
		}
	}
}
