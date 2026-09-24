package attachments

import "testing"

// BUG-3186: the path-boundary rule. Stored names are never passed through it;
// this is only what a saver on Windows turns a name into.
func TestWindowsPathComponent_BUG3186(t *testing.T) {
	cases := []struct{ in, want string }{
		// The filing's cases: a colon names an alternate data stream.
		{"Meeting: notes.pdf", "Meeting_ notes.pdf"},
		{"a.svg:x.txt", "a.svg_x.txt"},
		{"What?.png", "What_.png"},
		{`a<b>c"d|e*f.txt`, "a_b_c_d_e_f.txt"},
		{"tab\there.txt", "tab_here.txt"},
		// Windows strips trailing dots and spaces silently.
		{"report. ", "report"},
		{"photo.", "photo"},
		// The device rule still applies, after the replacement.
		{"nul.txt", "_nul.txt"},
		{"CON", "_CON"},
		{"nul:x.txt", "nul_x.txt"}, // the colon is replaced first, so the stem is no longer "nul"
		// Unchanged.
		{"ordinary name.pdf", "ordinary name.pdf"},
		{"résumé.pdf", "résumé.pdf"},
		// Nothing usable survives: the caller's fallback applies.
		{"...", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := WindowsPathComponent(c.in); got != c.want {
			t.Errorf("WindowsPathComponent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
