package links

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// BUG-2832: Go comments describe the web renderer's behaviour constantly, and
// nothing verifies those references. They cross a language boundary, so no
// compiler, test or linter has ever checked them — and they had drifted: seven
// citations across four files pointed at unrelated code, one of them
// substantively wrong in a way that got quoted forward into a bug repro
// (TASK-2826) and understated BUG-2805's severity.
//
// Line numbers cannot be checked. Symbol names can, and that is the whole
// reason the citations were converted to `markdown.ts::<symbol>` form. This
// file is the check that makes the conversion worth something: rename or delete
// a cited symbol in markdown.ts and the Go build's tests go red, naming the
// stale citation.
//
// It is a cheap, coarse instrument on purpose — a substring search for a
// declaration, not a TypeScript parse. It catches the failure that actually
// happens (a symbol is renamed or removed) without pulling a TS toolchain into
// `go test`.

const markdownTSRelPath = "../../web/src/lib/utils/markdown.ts"

// repoRootRelPath is where the sweep starts. internal/links -> repo root.
const repoRootRelPath = "../.."

var (
	// A citation of the form `markdown.ts::` + an identifier. The
	// captured symbol stops at the first character that cannot be part of a JS
	// identifier, so trailing prose ("::resolveWikiBody step 2") is ignored.
	symbolCitation = regexp.MustCompile(`markdown\.ts::([A-Za-z_$][A-Za-z0-9_$]*)`)

	// The form this test BANS. A line number is unverifiable by construction:
	// it is correct only until anyone inserts a line above it, and nothing
	// anywhere reports when that happens.
	lineCitation = regexp.MustCompile(`markdown\.ts:\d`)
)

func goSourceFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(repoRootRelPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "web", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(out) < 50 {
		// The sweep is the instrument; if it stops finding files it would
		// report a clean bill of health having examined nothing.
		t.Fatalf("sweep found only %d Go files — the walk is broken, not the citations", len(out))
	}
	return out
}

// TestMarkdownCitationsNameLiveSymbols is the mechanical check line numbers
// could never have.
func TestMarkdownCitationsNameLiveSymbols(t *testing.T) {
	t.Parallel()

	tsBytes, err := os.ReadFile(filepath.Clean(markdownTSRelPath))
	if err != nil {
		t.Fatalf("read markdown.ts: %v", err)
	}
	ts := string(tsBytes)

	// Where each cited symbol was found, so a failure names the file to fix.
	citedIn := map[string][]string{}
	for _, f := range goSourceFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range symbolCitation.FindAllStringSubmatch(string(b), -1) {
			citedIn[m[1]] = append(citedIn[m[1]], f)
		}
	}

	if len(citedIn) == 0 {
		t.Fatal("no `markdown.ts::` symbol citations found anywhere — either the " +
			"citation form changed or the sweep is broken; both make this test vacuous")
	}

	symbols := make([]string, 0, len(citedIn))
	for s := range citedIn {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)

	for _, sym := range symbols {
		// A declaration, not a mere mention: `function foo`, `const foo`,
		// `export function foo`, etc. Requiring the keyword is what stops a
		// citation from being "verified" by the very comment that cites it.
		//
		// The trailing character class is load-bearing and was added after this
		// check FAILED its negative control. The first version asked
		// `strings.Contains(ts, "function "+sym)`, which is a PREFIX match:
		// renaming `resolveWikiBody` to `resolveWikiBodyRENAMED` left
		// "function resolveWikiBody" a substring of the renamed declaration, so
		// the guard stayed green through exactly the rename it exists to catch.
		// A guard that passes on its own target is worse than no guard, because
		// it is also a claim that someone checked.
		if !declaredIn(ts, sym) {
			files := citedIn[sym]
			sort.Strings(files)
			t.Errorf("Go comments cite `markdown.ts::%s`, but markdown.ts declares no such symbol.\n"+
				"  cited from: %s\n"+
				"Either the symbol was renamed or removed on the web side and these comments now "+
				"describe code that does not exist, or the citation was wrong when written. "+
				"Fix the comments — do not weaken this check.",
				sym, strings.Join(dedupe(files), ", "))
		}
	}

	t.Logf("verified %d distinct cited symbols across %d citation sites", len(symbols), countSites(citedIn))
}

// TestMarkdownCitationsAreNotLineNumbers keeps the fix from eroding.
//
// Converting the citations is worth nothing if the next comment reaches for a
// line number again, and a line number is exactly what a person reads off their
// editor's gutter. This is the guard that makes the symbol form the path of
// least resistance.
func TestMarkdownCitationsAreNotLineNumbers(t *testing.T) {
	t.Parallel()

	var offenders []string
	for _, f := range goSourceFiles(t) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if lineCitation.MatchString(line) {
				offenders = append(offenders, filepath.ToSlash(f)+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("Go comments cite markdown.ts by LINE NUMBER in %d place(s):\n  %s\n\n"+
			"Nothing verifies a cross-language line citation, and they drift silently — "+
			"BUG-2832 found seven that had rotted onto unrelated code. Cite the symbol "+
			"instead (`markdown.ts::resolveWikiBody`), which "+
			"TestMarkdownCitationsNameLiveSymbols can actually check.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// declaredIn reports whether markdown.ts declares `sym` as its own symbol.
//
// The symbol must be followed by something that cannot continue a JavaScript
// identifier (or by end-of-input), so a citation is not satisfied by a LONGER
// declaration that merely starts with it. See the note at the call site: the
// prefix-matching version of this check survived its own negative control.
func declaredIn(ts, sym string) bool {
	re := regexp.MustCompile(
		`(?:function|const|let|var|class|type|interface)\s+` +
			regexp.QuoteMeta(sym) +
			`(?:[^A-Za-z0-9_$]|$)`)
	return re.MatchString(ts)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func countSites(m map[string][]string) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
