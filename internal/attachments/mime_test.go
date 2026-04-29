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
