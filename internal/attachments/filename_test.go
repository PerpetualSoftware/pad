package attachments

import (
	"path/filepath"
	"strings"
	"testing"
)

// svgHead is an SVG document with a script, which http.DetectContentType
// sniffs as text/xml: an ALLOWED type. The extension is therefore the only
// thing that refuses it, which is what makes an extension bypass a bypass.
var svgHead = []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)

func TestNormalizeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		// Unchanged: ordinary names in any script.
		{"shot.png", "shot.png"},
		{"日本語のメモ.txt", "日本語のメモ.txt"},
		{"a b.pdf", "a b.pdf"},
		{"...hidden", "...hidden"},
		// BUG-2818: every character the header drops is dropped HERE, so the
		// extension the blocklist judges is the one the header serves.
		{"x.s\vvg", "x.svg"},
		{"x.s\x08vg", "x.svg"},
		{"x.s\u0085vg", "x.svg"}, // C1: a control character too
		{"x.s\x7fvg", "x.svg"},
		{`x.s"vg`, "x.svg"},
		// BUG-3153: every Bidi_Control is dropped. The spoof itself — a .txt
		// that a bidi-aware renderer shows as "xtxt.svg" — keeps its letters
		// and its real extension.
		{"x\u202Egvs.txt", "xgvs.txt"},
		{"x\u202Agvs\u202C.txt", "xgvs.txt"},
		{"x\u2066a\u2067b\u2068c\u2069.txt", "xabc.txt"},
		{"a\u200Eb\u200Fc\u061Cd.txt", "abcd.txt"},
		// One inside the extension hid it from the blocklist, as a control
		// byte did.
		{"x.s\u202Evg", "x.svg"},
		// ZWJ is not Bidi_Control: an emoji sequence survives intact.
		{"\U0001F469\u200D\U0001F4BB notes.txt", "\U0001F469\u200D\U0001F4BB notes.txt"},
		// Nor are other format characters touched (U+00AD soft hyphen, Cf).
		{"co\u00ADop.txt", "co\u00ADop.txt"},
		// Trailing dots and spaces, which a download save strips (Q1).
		{"x.svg.", "x.svg"},
		{"x.svg ", "x.svg"},
		{"x.svg . .", "x.svg"},
		{"x.svg\t.", "x.svg"},
		// Leaf reduction under both separators, as the upload door always did.
		{"a/b/c.png", "c.png"},
		{`..\..\evil.png`, "evil.png"},
		// A backslash cut is a SEPARATOR first: `x.s\vg` is the leaf "vg",
		// never "x.svg".
		{`x.s\vg`, "vg"},
		// Nothing left: the generic name.
		{"", "upload.bin"},
		{".", "upload.bin"},
		{"..", "upload.bin"},
		{"/", "upload.bin"},
		{"\v\v", "upload.bin"},
		// All dots is a POSIX name, not a path component (BUG-2803 round 27).
		{"...", "..."},
		{". .", ". ."},
		// BUG-2822: a Windows device name, with or without an extension, in any
		// case, and with the stem's trailing spaces ignored as Windows ignores
		// them, gets a "_" prefix. The name stays readable, and so does its
		// extension.
		{"CON", "_CON"},
		{"nul.txt", "_nul.txt"},
		{"Aux.tar.gz", "_Aux.tar.gz"},
		{"con .txt", "_con .txt"},
		{"PRN.", "_PRN"},
		{"LPT1.png", "_LPT1.png"},
		{"com9", "_com9"},
		{"COM0.txt", "_COM0.txt"},
		{"com\u00B9.txt", "_com\u00B9.txt"},
		{"CONIN$", "_CONIN$"},
		{"conout$.log", "_conout$.log"},
		{"a/b/NUL", "_NUL"},
		{"NUL:stream.txt", "_NUL:stream.txt"},
		{"con:", "_con:"},
		// Preservation: a device name inside a longer stem, after the first
		// dot, or with a leading space, is not one. Nor are COM and LPT
		// without a digit, or with two.
		{"console.txt", "console.txt"},
		{"connect.png", "connect.png"},
		{"nullable.go", "nullable.go"},
		{"report.con", "report.con"},
		{"my.nul.txt", "my.nul.txt"},
		{" con.txt", " con.txt"},
		{"com.txt", "com.txt"},
		{"COM10.txt", "COM10.txt"},
		{"lpt.png", "lpt.png"},
		{"auxiliary.pdf", "auxiliary.pdf"},
		{"note:con.txt", "note:con.txt"},
		// Unstorable text keeps BUG-2803's fallback, extension only if allowed.
		{"sh\x00ot.png", "upload.png"},
		{"sh\x00ot.svg", "upload"},
		{"bad\xffname.png", "upload.png"},
	}
	for _, c := range cases {
		if got := NormalizeFilename(c.in); got != c.want {
			t.Errorf("NormalizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The adversarial property, per attack name. The raw name PASSES the blocklist
// (the counterfactual: this is the bypass, measured rather than asserted), and
// the normalised name is refused as extension_blocked.
func TestNormalizedAttackNamesAreRefusedByTheBlocklist(t *testing.T) {
	attacks := []string{
		"x.s\vvg", "x.s\x08vg", "x.s\u0085vg", "x.s\x7fvg", `x.s"vg`,
		"x.svg.", "x.svg ", "x.svg\t.", "x.e\vxe", "x.b\"at",
	}
	for _, raw := range attacks {
		if _, code, err := ValidateUpload(svgHead, raw); err != nil {
			t.Errorf("counterfactual %q: the raw name was already refused (%s), so this case proves nothing", raw, code)
		}
		name := NormalizeFilename(raw)
		if _, code, err := ValidateUpload(svgHead, name); err == nil || code != "extension_blocked" {
			t.Errorf("%q normalised to %q, which ValidateUpload answered %q (%v); want extension_blocked", raw, name, code, err)
		}
	}
}

func TestServedFilename(t *testing.T) {
	cases := []struct{ stored, want string }{
		{"shot.png", "shot.png"},
		{"report.pdf", "report.pdf"},
		// Legacy rows stored before BUG-2818 whose normalised extension is
		// blocked: served as .bin, never as the blocked type.
		{"x.s\vvg", "x.bin"},
		{`x.s"vg`, "x.bin"},
		{"setup.e\vxe", "setup.bin"},
		{"x.svg.", "x.bin"},
		{".s\vvg", "attachment.bin"},
		// A legacy bundle-import name carrying a backslash reduces to its leaf.
		{`x.s\vg`, "vg"},
	}
	for _, c := range cases {
		got := ServedFilename(c.stored)
		if got != c.want {
			t.Errorf("ServedFilename(%q) = %q, want %q", c.stored, got, c.want)
		}
		if BlockedExtension(filepath.Ext(got)) {
			t.Errorf("ServedFilename(%q) = %q serves a blocked extension", c.stored, got)
		}
	}
}

func TestBlockedExtensionAgreesWithValidateUpload(t *testing.T) {
	// Every mapped extension: blocked iff ValidateUpload refuses it as
	// extension_blocked on bytes it would otherwise accept. Two readings of
	// one map, checked against each other rather than against a list.
	for ext := range extMIMEMap {
		_, code, _ := ValidateUpload(svgHead, "f"+ext)
		if got, want := BlockedExtension(ext), code == "extension_blocked"; got != want {
			t.Errorf("BlockedExtension(%q) = %v, but ValidateUpload answered %q", ext, got, code)
		}
		if BlockedExtension(ext) != BlockedExtension(strings.ToUpper(ext)) {
			t.Errorf("BlockedExtension(%q) depends on case", ext)
		}
	}
	if BlockedExtension(".nosuchext") || BlockedExtension("") {
		t.Error("an unknown or empty extension is not blocked")
	}
}
