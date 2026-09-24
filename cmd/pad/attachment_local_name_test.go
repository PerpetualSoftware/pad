package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// BUG-3186: the CLI applies the Windows path rule only when it runs on
// Windows. On Linux and macOS the served name is kept, since a colon is legal
// there; the NTFS-mount edge is accepted (see safeLocalFilenameFor).
func TestSafeLocalFilenameFor_BUG3186(t *testing.T) {
	cases := []struct{ goos, in, want string }{
		{"windows", "Meeting: notes.pdf", "Meeting_ notes.pdf"},
		{"windows", "a.svg:x.txt", "a.svg_x.txt"},
		{"windows", "What?.png", "What_.png"},
		{"windows", "nul.txt", "_nul.txt"}, // an older server can still serve a device name
		{"windows", "ordinary.pdf", "ordinary.pdf"},
		// The same names off Windows: kept as given.
		{"linux", "Meeting: notes.pdf", "Meeting: notes.pdf"},
		{"darwin", "What?.png", "What?.png"},
		{"linux", "nul.txt", "nul.txt"},
		// The pre-existing guards hold on every OS.
		{"linux", "../escape.txt", "escape.txt"},
		{"windows", "..", ""},
		{"windows", "photo.", "photo"},
		// A name that is ALL reserved characters, which the Windows rule turns
		// into underscores rather than emptying.
		{"windows", "???", "___"},
	}
	for _, c := range cases {
		if got := safeLocalFilenameFor(c.goos, c.in); got != c.want {
			t.Errorf("safeLocalFilenameFor(%q, %q) = %q, want %q", c.goos, c.in, got, c.want)
		}
	}
}

// The binding leg, driven through the PRODUCTION wrapper under each OS. A
// wrapper that ignores hostOS (hard-coding "windows" or "linux") gives the
// same answer for both runs, and one of them fails, on any build host.
func TestSafeLocalFilename_FollowsHostOS_BUG3186(t *testing.T) {
	saved := hostOS
	t.Cleanup(func() { hostOS = saved })

	hostOS = "windows"
	if got := safeLocalFilename("Meeting: notes.pdf"); got != "Meeting_ notes.pdf" {
		t.Errorf("on windows: got %q, want %q", got, "Meeting_ notes.pdf")
	}
	hostOS = "linux"
	if got := safeLocalFilename("Meeting: notes.pdf"); got != "Meeting: notes.pdf" {
		t.Errorf("on linux: got %q, want %q", got, "Meeting: notes.pdf")
	}
}

// hostOS must be initialised from runtime.GOOS itself. A behavioural check
// cannot see a wrong DEFAULT on a Linux runner ("linux" == runtime.GOOS there),
// and no Go tests run on Windows in CI (the Windows smoke job only builds and
// runs --help), so the initialiser is checked in the source, where it is
// exactly one expression.
func TestHostOSIsRuntimeGOOS_BUG3186(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "cmd_attachment.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.VAR {
			continue
		}
		for _, sp := range g.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != "hostOS" {
					continue
				}
				if i >= len(vs.Values) {
					t.Fatal("hostOS has no initialiser")
				}
				sel, ok := vs.Values[i].(*ast.SelectorExpr)
				if !ok {
					t.Fatal("hostOS must be initialised from runtime.GOOS")
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "runtime" || sel.Sel.Name != "GOOS" {
					t.Fatal("hostOS must be initialised from runtime.GOOS")
				}
				return
			}
		}
	}
	t.Fatal("precondition: var hostOS not found in cmd_attachment.go")
}
