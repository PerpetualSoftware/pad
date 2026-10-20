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

// bug3968Time is the best of three ExtractWikiLinks runs over body, failing
// at once past a backstop so a quadratic scan cannot run the suite for minutes.
func bug3968Time(t *testing.T, n int) time.Duration {
	t.Helper()
	body := bug3968Body(n)
	best := time.Duration(0)
	for i := 0; i < 3; i++ {
		start := time.Now()
		got := ExtractWikiLinks(body)
		d := time.Since(start)
		if len(got) != n {
			t.Fatalf("n=%d: got %d links", n, len(got))
		}
		if d > 30*time.Second {
			t.Fatalf("n=%d: one run took %v; a per-match range scan is back", n, d)
		}
		if best == 0 || d < best {
			best = d
		}
	}
	return best
}

// The code-range check walks the spans once per call, not once per match
// (BUG-3968). Relative, not an absolute bound, so a slow runner (-race, a
// loaded box) cannot fail it (BUG-3995): 4x the input costs about 4x when
// linear and about 16x when quadratic; the bound sits between them.
func TestExtractWikiLinksLinearAfterCodeSpans(t *testing.T) {
	const n = 20000
	small, large := bug3968Time(t, n), bug3968Time(t, 4*n)
	ratio := float64(large) / float64(small)
	t.Logf("n=%d: %v; n=%d: %v; ratio %.1f", n, small, 4*n, large, ratio)
	if ratio >= 8 {
		t.Fatalf("4x the input took %.1fx the time (%v -> %v); a per-match range scan is back", ratio, small, large)
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
