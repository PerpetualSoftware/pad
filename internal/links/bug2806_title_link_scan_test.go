package links

import (
	"math/rand"
	"strings"
	"testing"
)

// forEachTitleLink claims to be wikiLinkPattern executed by hand, including
// the skip-to-the-stop step its linearity depends on. This drives it against
// the regex itself (plus splitOnUnescapedPipe and unescapeWikiBody, the rule
// ReplaceTitle documents) on random strings over the grammar's own alphabet,
// where every special case lives.
func TestForEachTitleLinkMatchesTheRegexOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(2806))
	// Tokens rather than bytes, so brackets and escapes actually form: a byte
	// alphabet reached only 91 matches in 20,000 strings.
	tokens := []string{"[[", "]]", "[", "]", `\`, "|", "a", "A", " ", `\]`, `\|`, `\\`, "[[a]]", "[[a\\a]]", "[[a\\|a]]"}
	titles := []string{"a", "A", "a]", "a|a", `a\a`, `a\`, "[a", "aa", " a"}
	checked := 0
	for n := 0; n < 20000; n++ {
		var sb strings.Builder
		for i := rng.Intn(10); i >= 0; i-- {
			sb.WriteString(tokens[rng.Intn(len(tokens))])
		}
		content := sb.String()
		for _, title := range titles {
			var want [][2]int
			for _, m := range wikiLinkPattern.FindAllStringSubmatchIndex(content, -1) {
				body := content[m[2]:m[3]]
				if _, _, piped := splitOnUnescapedPipe(body); !piped && unescapeWikiBody(body) == title {
					want = append(want, [2]int{m[0], m[1]})
				}
			}
			var got [][2]int
			forEachTitleLink(content, title, func(s, e int) { got = append(got, [2]int{s, e}) })
			if len(got) != len(want) {
				t.Fatalf("content %q title %q: got %v, oracle %v", content, title, got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("content %q title %q: got %v, oracle %v", content, title, got, want)
				}
			}
			checked += len(want)
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d matches exercised; the generator is not reaching the grammar", checked)
	}
}

// ProjectReplaceTitle must agree with what ReplaceTitle builds.
func TestProjectReplaceTitleIsReplaceTitlesLength(t *testing.T) {
	for _, c := range []struct{ content, old, new string }{
		{`x [[A\|B]] [[A|B]] y`, `A|B`, "Fresh"},
		{`[[A\B]] [[A\\B]]`, `A\B`, `C]D`},
		{strings.Repeat("[[A]]", 50), "A", strings.Repeat("n", 40)},
		{"no links", "A", "B"},
	} {
		n, occ := ProjectReplaceTitle(c.content, c.old, c.new)
		if got := ReplaceTitle(c.content, c.old, c.new); n != len(got) {
			t.Errorf("%q: projected %d (%d links), built %d", c.content, n, occ, len(got))
		}
	}
}
