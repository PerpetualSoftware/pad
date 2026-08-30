package main

import (
	"path/filepath"
	"testing"
)

// safeLocalFilename builds a LOCAL path component out of a remote string, so
// its contract is what keeps `pad attachment view` from writing outside its
// temp dir or handing viewers a name they cannot dispatch on. The CLI guard
// had no tests of its own (codex closing round, BUG-2803); each case below
// names the failure it discriminates.
func TestSafeLocalFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary name kept", "photo.png", "photo.png"},
		{"whitespace trimmed", "  photo.png \t", "photo.png"},
		{"empty refused", "", ""},
		{"dot refused", ".", ""},
		// filepath.Ext("..") is "." — non-empty — so an extension check waves
		// it through, and filepath.Join(dir, "..") is dir's PARENT.
		{"dot-dot refused", "..", ""},
		{"dots-only refused", "...", ""},
		{"dots-only long refused", "....", ""},
		// Base() strips directories, so a path arrives as its last segment.
		{"path reduced to base", "a/b/photo.png", "photo.png"},
		// A backslash is a legitimate byte in a Unix filename, but this name
		// is written to the LOCAL filesystem, which may read it as a
		// separator. Refusal (fall back to the attachment id) is deliberate.
		{"backslash refused", `a\b.png`, ""},
		{"traversal via backslash refused", `..\evil.png`, ""},
		// A trailing dot makes filepath.Ext answer "." — non-empty — so the
		// caller's MIME-extension fallback never fires and the temp file
		// dispatches on no extension; Windows cannot store the name at all.
		{"trailing dot stripped", "photo.", "photo"},
		{"trailing dots stripped", "archive.tar..", "archive.tar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeLocalFilename(tc.in); got != tc.want {
				t.Fatalf("safeLocalFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// The property the trailing-dot rule exists for, stated as the caller
	// consumes it: a stripped name must read as EXTENSIONLESS so the
	// MIME-extension fallback in attachmentViewCmd fires. On the unfixed
	// code safeLocalFilename("photo.") returned "photo." whose Ext is ".",
	// and the fallback stayed silent.
	if got := safeLocalFilename("photo."); filepath.Ext(got) != "" {
		t.Fatalf("safeLocalFilename(%q) = %q still carries extension %q; the MIME fallback will not fire",
			"photo.", got, filepath.Ext(got))
	}
}
