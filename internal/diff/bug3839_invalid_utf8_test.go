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
			// The web UI and the module's own tooling dirs, at the root only:
			// a Go package named web elsewhere is still production code.
			if filepath.Dir(path) == root {
				switch d.Name() {
				case "node_modules", ".git", "web", "vendor":
					return filepath.SkipDir
				}
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
		// Inside the package, ReversePatch and CreateReversePatch are the
		// checked door and its unchecked twin.
		if f.Name.Name == "diff" && filepath.Base(filepath.Dir(path)) == "diff" {
			return nil
		}
		bad = append(bad, uncheckedPatchUses(fset, f)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("production code uses an unchecked patch builder; use diff.ReversePatch (BUG-3839):\n%s", strings.Join(bad, "\n"))
	}
}

const diffImportPath = "github.com/PerpetualSoftware/pad/internal/diff"

// uncheckedPatchUses lists every reference to CreatePatch or
// CreateReversePatch through this file's import of the package: a call, a
// method value, under any import name, or bare under a dot import.
func uncheckedPatchUses(fset *token.FileSet, f *ast.File) []string {
	local, dot := "", false
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != diffImportPath {
			continue
		}
		switch {
		case imp.Name == nil:
			local = "diff"
		case imp.Name.Name == ".":
			dot = true
		case imp.Name.Name != "_":
			local = imp.Name.Name
		}
	}
	if local == "" && !dot {
		return nil
	}
	unchecked := func(name string) bool { return name == "CreatePatch" || name == "CreateReversePatch" }
	var out []string
	// A selector's own name is never a dot-imported identifier.
	sel := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			sel[x.Sel] = true
			// A selector on a local variable that shadows the import name is
			// a false positive here; none exists, and a hit says which file.
			if id, ok := x.X.(*ast.Ident); ok && local != "" && id.Name == local && unchecked(x.Sel.Name) {
				out = append(out, fset.Position(x.Pos()).String()+": "+x.Sel.Name)
			}
		case *ast.Ident:
			if dot && !sel[x] && unchecked(x.Name) {
				out = append(out, fset.Position(x.Pos()).String()+": "+x.Name)
			}
		}
		return true
	})
	return out
}

// The census's own instrument: each way a file can reach the unchecked
// builders is found, and the checked door and a same-named local are not.
func TestBUG3839_UncheckedPatchCensusFindsEveryForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		hits int
	}{
		{"call", `import "` + diffImportPath + `"; func f() { diff.CreatePatch("a", "b") }`, 1},
		{"method_value", `import "` + diffImportPath + `"; var g = diff.CreateReversePatch`, 1},
		{"alias", `import d "` + diffImportPath + `"; func f() { d.CreatePatch("a", "b") }`, 1},
		{"dot", `import . "` + diffImportPath + `"; func f() { CreateReversePatch("a", "b"); _ = CreatePatch }`, 2},
		{"dot_inside_a_selector", `import . "` + diffImportPath + `"; func f() { _ = T{CreatePatch("a", "b")}.x; _ = other.CreatePatch }`, 1},
		{"checked_door", `import "` + diffImportPath + `"; func f() { diff.ReversePatch("a", "b") }`, 0},
		{"other_package_named_diff", `import "example.com/diff"; func f() { diff.CreatePatch("a", "b") }`, 0},
		{"alias_frees_the_name", `import d "` + diffImportPath + `"; func f(diff T) { diff.CreatePatch() }`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", "package x\n"+tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := uncheckedPatchUses(fset, f); len(got) != tc.hits {
				t.Fatalf("found %d uses, want %d: %v", len(got), tc.hits, got)
			}
		})
	}
}
