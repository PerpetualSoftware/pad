package links

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Go half of the cross-language wiki-link grammar harness (BUG-2834).
//
// The JS half is web/src/lib/utils/markdown.grammarParity.test.ts. Both read
// the SAME corpus and assert against the expectations recorded IN it, so
// neither language's current behaviour is what the other is measured against.
// That indirection is the whole point: the two patterns are byte-identical
// source text, so a reviewer comparing them concludes they agree, and BUG-2834
// lived entirely in the host languages' disagreement about what `.` means.
// Comparing implementation to implementation would have reproduced the same
// blind spot in test form.
//
// If you add a case, add it to the corpus — not to one language's test.

const grammarCorpusRelPath = "../../testdata/wiki_grammar_corpus.json"

type grammarExpect struct {
	Match bool   `json:"match"`
	Body  string `json:"body"`
}

type grammarCase struct {
	Name    string        `json:"name"`
	Why     string        `json:"why"`
	Content string        `json:"content"`
	Expect  grammarExpect `json:"expect"`
}

type grammarCorpus struct {
	Cases []grammarCase `json:"cases"`
}

func loadGrammarCorpus(t *testing.T) grammarCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(grammarCorpusRelPath))
	if err != nil {
		t.Fatalf("read shared grammar corpus: %v", err)
	}
	var corpus grammarCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse shared grammar corpus: %v", err)
	}
	// An empty or unparsed corpus would make every assertion below vacuous and
	// the suite would report PASS having measured nothing. The count is asserted
	// rather than assumed for the same reason the corpus carries its own
	// expectations: a harness that cannot fail is not an instrument.
	if len(corpus.Cases) < 20 {
		t.Fatalf("shared grammar corpus looks truncated: %d cases (expected >= 20)", len(corpus.Cases))
	}
	return corpus
}

// TestWikiLinkGrammarMatchesSharedCorpus drives the REAL wikiLinkPattern — not
// a copy of its source text — over the shared corpus.
func TestWikiLinkGrammarMatchesSharedCorpus(t *testing.T) {
	t.Parallel()
	corpus := loadGrammarCorpus(t)

	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			m := wikiLinkPattern.FindStringSubmatch(tc.Content)

			if !tc.Expect.Match {
				if m != nil {
					t.Fatalf("expected NO match but Go matched, body=%s\ncontent=%s\nwhy this case exists: %s",
						quoteCodePoints(m[1]), quoteCodePoints(tc.Content), tc.Why)
				}
				return
			}
			if m == nil {
				t.Fatalf("expected a match, Go found none\ncontent=%s\nwhy this case exists: %s",
					quoteCodePoints(tc.Content), tc.Why)
			}
			if m[1] != tc.Expect.Body {
				t.Fatalf("captured body mismatch\n got: %s\nwant: %s\ncontent=%s\nwhy this case exists: %s",
					quoteCodePoints(m[1]), quoteCodePoints(tc.Expect.Body),
					quoteCodePoints(tc.Content), tc.Why)
			}
		})
	}
}

// quoteCodePoints renders a string with every non-printable rune as \uXXXX.
//
// %q alone is not enough here: this corpus is ENTIRELY about characters that
// are invisible or that terminate a line in a terminal, and a failure message
// that prints a raw U+2028 is a failure message that lies about what it
// compared. The bug being tested is itself a case of an invisible character
// being mistaken for something else.
func quoteCodePoints(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	b.WriteByte('"')
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 {
			fmt.Fprintf(&b, `\u%04X`, r)
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}
