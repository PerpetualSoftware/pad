package main

import "testing"

// BUG-3185: the CLI writes the export file itself, so it applies the device
// rule to the name it will write, whether that name came from the server
// (an older server still suggests "nul.pad.md") or from the ref fallback.
func TestDefaultExportPath_BUG3185(t *testing.T) {
	cases := []struct{ server, ref, want string }{
		// Server-suggested names.
		{"nul.pad.md", "PLAYB-1", "_nul.pad.md"},
		{"CON.pad.md", "PLAYB-1", "_CON.pad.md"},
		{"_nul.pad.md", "PLAYB-1", "_nul.pad.md"}, // a fixed server's name is not prefixed twice
		{"ship.pad.md", "PLAYB-1", "ship.pad.md"},
		// The ref fallback, when the header is absent.
		{"", "nul", "_nul.pad.md"},
		{"", "com1", "_com1.pad.md"},
		{"", "PLAYB-1", "PLAYB-1.pad.md"},
		{"", "nulls", "nulls.pad.md"},
	}
	for _, c := range cases {
		if got := defaultExportPath(c.server, c.ref); got != c.want {
			t.Errorf("defaultExportPath(%q, %q) = %q, want %q", c.server, c.ref, got, c.want)
		}
	}
}
