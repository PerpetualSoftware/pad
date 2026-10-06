package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// rebindQuery does not lex Postgres dollar-quoted strings ($$...$$,
// $tag$...$tag$) or E'...' escape strings, so a "?" inside either would be
// bound on Postgres and nowhere else (BUG-3430). No store SQL uses them. This
// guard makes the first one to appear fail here, instead of binding wrongly
// on Postgres only.
//
// It reads the Go string LITERALS of package store's non-test files (comments
// are not literals, so prose about these forms does not trip it). SQL in other
// packages that is executed directly, never through Store.q / Rebind (the
// storetest write-capture trigger, for one), is out of its reach and needs
// none of this.
var (
	dollarQuoteRE  = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)?\$`)
	escapeStringRE = regexp.MustCompile(`(^|[\s(,=])[Ee]'`)
)

func unsupportedRebindForm(lit string) string {
	if m := dollarQuoteRE.FindString(lit); m != "" {
		return "dollar quote " + m
	}
	if escapeStringRE.MatchString(lit) {
		return "E'...' escape string"
	}
	return ""
}

func TestStoreSQLAvoidsFormsRebindDoesNotLex(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if form := unsupportedRebindForm(val); form != "" {
				t.Errorf("%s: a string literal holds a %s, which rebindQuery does not lex (a ? inside it would be bound on Postgres). Use a plain '...' literal or bind the value.",
					fset.Position(lit.Pos()), form)
			}
			return true
		})
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files; the glob is not reading package store", scanned)
	}
}

// The detector itself, so the guard cannot pass by matching nothing.
func TestUnsupportedRebindFormDetector(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"SELECT $$?$$", true},
		{"AS $body$ x $body$", true},
		{"SELECT E'can\\'t'", true},
		{"WHERE x = e'a'", true},
		{"(E'x')", true},
		{"SELECT 'TYPE', ?", false},
		{"WHERE name = 'E' AND x = ?", false},
		{"SELECT $1, $2", false},
		{"WHERE note LIKE '%e'", false},
	} {
		if got := unsupportedRebindForm(tc.in) != ""; got != tc.want {
			t.Errorf("unsupportedRebindForm(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
