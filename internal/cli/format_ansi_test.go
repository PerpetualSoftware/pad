package cli

import (
	"strings"
	"testing"
)

// #1076: "ANSI stripping" covered only SGR (ESC[…m), so non-SGR CSI sequences,
// OSC-8 hyperlinks, and stray C0 controls survived — including through the
// markdown renderers, whose doc comments promise ANSI is stripped.

func TestStripANSI_NonSGRSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"SGR colour", "\x1b[1;31mred\x1b[0m", "red"},
		{"CSI erase line", "before\x1b[2Kafter", "beforeafter"},
		{"CSI cursor move", "a\x1b[10;20Hb", "ab"},
		{"CSI private mode", "a\x1b[?25lb", "ab"},
		{"OSC-8 hyperlink", "\x1b]8;;http://example.com\x07label\x1b]8;;\x07", "label"},
		{"OSC window title with ST", "a\x1b]0;title\x1b\\b", "ab"},
		{"stray C0 control", "a\x01b\x7fc", "abc"},
		{"vertical tab and form feed", "a\x0bb\x0cc", "abc"},
		{"bare ESC", "a\x1bb", "ab"},
		{"plain text untouched", "nothing to strip", "nothing to strip"},
		{"newlines preserved for the caller to handle", "a\nb", "a\nb"},
		{"tabs preserved", "a\tb", "a\tb"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripANSI(tc.in); got != tc.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// displayWidth drives the table column maths, so a zero-width control sequence
// must not be counted as visible columns.
func TestDisplayWidth_IgnoresNonSGRSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"CSI erase line", "abc\x1b[2K", 3},
		{"OSC-8 hyperlink", "\x1b]8;;http://example.com\x07abc\x1b]8;;\x07", 3},
		{"stray C0", "ab\x01c", 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayWidth(tc.in); got != tc.want {
				t.Errorf("displayWidth(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// The markdown path promises no ANSI in its output; that promise has to hold for
// non-SGR sequences too.
func TestSanitizeMarkdownText_StripsNonSGRSequences(t *testing.T) {
	in := "Tasks\x1b[2K\x1b]8;;http://example.com\x07"
	got := SanitizeMarkdownText(in)

	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("escape byte survived: %q", got)
	}
	if got != "Tasks" {
		t.Errorf("SanitizeMarkdownText(%q) = %q, want %q", in, got, "Tasks")
	}
}

func TestEscapeMarkdownCell_StripsNonSGRSequences(t *testing.T) {
	got := escapeMarkdownCell("a\x1b[2Kb")
	if got != "ab" {
		t.Errorf("escapeMarkdownCell = %q, want %q", got, "ab")
	}
}
