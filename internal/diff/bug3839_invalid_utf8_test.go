package diff

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-3839: go-diff cannot patch text that is not valid UTF-8. Near an edit
// it panics or builds a patch that does not reproduce the old text; both
// shapes are pinned below through CreateReversePatch (unchecked), and
// ReversePatch refuses them.
var bug3839Cases = []struct {
	name     string
	old, new string
}{
	// Panic shapes: an invalid byte, lone or a truncated sequence, next to
	// an edit.
	{"lone_ff", "a\xff", "a\xffb"},
	{"lone_80", "x\x80y", "\x80"},
	{"truncated_2", "a\xc3", "a\xc3b"},
	{"truncated_3", "\xe2\x82", "x\xe2\x82y"},
	// Wrong-patch shape: reverse-applies to "hello d", with no error.
	{"wrong_patch", "hello world", "hello \xff world"},
}

func TestBUG3839_ReversePatchRefusesInvalidUTF8(t *testing.T) {
	for _, tc := range bug3839Cases {
		t.Run(tc.name, func(t *testing.T) {
			if p, ok := ReversePatch(tc.old, tc.new); ok {
				t.Fatalf("ReversePatch accepted text that is not valid UTF-8: %q", p)
			}
			// Instrument: unchecked, each case really does fail.
			var p string
			panicked := func() (r any) {
				defer func() { r = recover() }()
				p = CreateReversePatch(tc.old, tc.new)
				return nil
			}()
			if panicked == nil {
				if got, err := ApplyPatch(tc.new, p); err == nil && got == tc.old {
					t.Fatalf("instrument: the unchecked patch round-trips, so this case shows nothing")
				}
			}
		})
	}
}

func TestBUG3839_ReversePatchRoundTripsValidText(t *testing.T) {
	for _, tc := range [][2]string{
		{"hello world", "hello brave world"},
		{"café ☕ 😀 naïve", "café ☕ naïve 😀!"},
		{"", "new"},
		{"old", ""},
	} {
		p, ok := ReversePatch(tc[0], tc[1])
		if !ok {
			t.Fatalf("ReversePatch refused valid text %q -> %q", tc[0], tc[1])
		}
		if got, err := ApplyPatch(tc[1], p); err != nil || got != tc[0] {
			t.Fatalf("%q -> %q: reverse-applied to %q (%v)", tc[0], tc[1], got, err)
		}
	}
}

// Production code calls ReversePatch, never the unchecked CreatePatch or
// CreateReversePatch, which tests still use to build fixtures from valid
// text. A census of every non-test Go file in the module.
func TestBUG3839_NoUncheckedPatchCallers(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var bad []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		inDiff := f.Name.Name == "diff" && filepath.Base(filepath.Dir(path)) == "diff"
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := fn.X.(*ast.Ident); ok && x.Name == "diff" {
					name = fn.Sel.Name
				}
			case *ast.Ident:
				if inDiff {
					name = fn.Name
				}
			}
			if name != "CreatePatch" && name != "CreateReversePatch" {
				return true
			}
			// Inside the package, ReversePatch and CreateReversePatch are
			// the checked door and its unchecked twin.
			if inDiff {
				return true
			}
			bad = append(bad, fset.Position(call.Pos()).String()+": "+name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("production code calls an unchecked patch builder; use diff.ReversePatch (BUG-3839):\n%s", strings.Join(bad, "\n"))
	}
}
