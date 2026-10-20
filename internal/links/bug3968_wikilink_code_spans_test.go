package links

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// bug3968Body is many inline code spans followed by many wiki-links: the
// shape that made a per-match range scan quadratic (BUG-3968).
func bug3968Body(n int) string {
	return strings.Repeat("`a` ", n) + strings.Repeat("[[x]] ", n)
}

// The code-range check walks the spans once per call, not once per match.
// Before BUG-3968, n=40000 (400 KB) took about 1.2 s, about 4x per doubling,
// so this 1.6 MB body took well over 3 s; linear, it takes milliseconds.
func TestExtractWikiLinksLinearAfterCodeSpans(t *testing.T) {
	body := bug3968Body(160000)
	start := time.Now()
	got := ExtractWikiLinks(body)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("ExtractWikiLinks on %d bytes took %v; a per-match range scan is back", len(body), d)
	}
	if len(got) != 160000 {
		t.Fatalf("got %d links, want 160000", len(got))
	}
}

// The cursor must give the same answers as a full scan: links before,
// between, inside and after inline and fenced code, and at span edges.
func TestExtractWikiLinksAroundCodeSpans(t *testing.T) {
	body := "[[A]] `[[B]]` [[C]]`x`[[D]]\n```\n[[E]]\n```\n[[F]] `y` `[[G]]` [[H]]"
	var got []string
	for _, r := range ExtractWikiLinks(body) {
		got = append(got, r.Title)
	}
	want := []string{"A", "C", "D", "F", "H"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func BenchmarkExtractWikiLinksAfterCodeSpans(b *testing.B) {
	for _, n := range []int{10000, 20000, 40000} {
		body := bug3968Body(n)
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				ExtractWikiLinks(body)
			}
		})
	}
}
