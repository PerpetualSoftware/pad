package links

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseMentions(t *testing.T) {
	long := strings.Repeat("a", 40)
	cases := []struct {
		name, in string
		want     []string
	}{
		{"empty", "", nil},
		{"plain", "hi @alice", []string{"alice"}},
		{"start of body", "@alice hi", []string{"alice"}},
		{"case folded and deduped", "@Alice @alice @ALICE", []string{"alice"}},
		{"sorted", "@zed and @bob", []string{"bob", "zed"}},
		{"trailing punctuation", "ping @alice, @bob. @carol!", []string{"alice", "bob", "carol"}},
		{"trailing hyphen trimmed", "@alice- ok", []string{"alice"}},
		{"hyphenated", "@a-b-c", []string{"a-b-c"}},
		{"double hyphen rejected", "@a--b", nil},
		{"too short", "@ab", nil},
		{"too long", "@" + long, nil},
		{"max length", "@" + strings.Repeat("a", 39), []string{strings.Repeat("a", 39)}},
		{"email skipped", "mail bob@example.com now", nil},
		{"email with plus", "a.b+c@example.com", nil},
		{"after paren", "(@alice)", []string{"alice"}},
		{"fenced", "```\n@alice\n```\n@bob", []string{"bob"}},
		{"inline code", "`@alice` and @bob", []string{"bob"}},
		{"leading hyphen run", "@-alice", nil},
		{"after slash is a separator", "see /@alice and x/@bob", []string{"alice", "bob"}},
		{"after backslash", `C:\@alice`, nil},
		{"underscore follower", "@alice_bot", nil},
		{"dot letter follower", "@alice.dev and @bob.2", nil},
		{"dot then space", "ping @alice. ok", []string{"alice"}},
		{"non-ascii follower", "@aliceé", nil},
		{"curly apostrophe", "@alice\u2019s", []string{"alice"}},
		{"nbsp follower", "@alice\u00a0ok", []string{"alice"}},
		{"ellipsis follower", "@alice\u2026", []string{"alice"}},
		{"em dash follower", "@alice\u2014", []string{"alice"}},
		{"emoji follower", "@alice\U0001F389", []string{"alice"}},
		{"fullwidth comma follower", "@alice\uff0c", []string{"alice"}},
		{"italics both sides", "_ping @alice_", []string{"alice"}},
		{"underscore then word rejected", "@alice_bob", nil},
		{"non-ascii letter rejected", "@aliceé", nil},
		{"dot capital rejected (documented limit)", "@alice.Thanks", nil},
		{"slash separates adjacent mentions", "cc @alice/@bob", []string{"alice", "bob"}},
		{"https url rejected", "https://x.social/@alice", nil},
		{"www url rejected", "www.x.social/@alice", nil},
		{"italics prefix", "_@alice_", []string{"alice"}},
		{"underscore prefix mid-word rejected", "foo_@alice", nil},
		{"url in an earlier run does not taint a later slash", "https://x.social/a and b/@alice", []string{"alice"}},
		{"url run with path then slash rejected", "https://x.social/a/@alice", nil},
		{"url then space then mention", "see https://x.social/a @alice", []string{"alice"}},
		{"www in an earlier run does not taint", "www.x.social z/@alice", []string{"alice"}},
		{"scheme earlier in the same run", "go:x://h/p/@alice", nil},
		{"mentions between and after code spans", "`a` @alice `@bob` @carol\n```\n@dave\n```\n@erin", []string{"alice", "carol", "erin"}},
		{"mention at a code span edge", "`x`@alice", []string{"alice"}},
		{"hyphen prefix rejected (documented limit)", "-@alice", nil},
		{"period at end", "thanks @alice.", []string{"alice"}},
		{"apostrophe", "@alice's turn", []string{"alice"}},
		{"parens", "(@alice)", []string{"alice"}},
		{"comma", "@alice, hi", []string{"alice"}},
		{"colon", "@alice: hi", []string{"alice"}},
	}
	for _, c := range cases {
		got := ExtractMentions(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ExtractMentions(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// ExtractMentions stays linear in the body: a backward scan per "@" made
// "/@a" repeated quadratic (TASK-3917 r3 F1; 240 KB took ~10 s). The bound is
// ~60x the linear cost, so only a superlinear walk can trip it.
func TestExtractMentionsLinearOnSlashRuns(t *testing.T) {
	body := strings.Repeat("/@a", 100000)
	start := time.Now()
	ExtractMentions(body)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("ExtractMentions on %d bytes took %v; a per-match backward scan is back", len(body), d)
	}
}

// The code-range check walks the spans once per call, not once per match: a
// body of many inline code spans followed by many mentions was quadratic
// (TASK-3917 r4; 360 KB took ~1.1 s, so a 2 MiB comment body ~40 s).
func TestExtractMentionsLinearAfterCodeSpans(t *testing.T) {
	body := strings.Repeat("`a` ", 160000) + strings.Repeat("@aaa ", 160000)
	start := time.Now()
	ExtractMentions(body)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("ExtractMentions on %d bytes took %v; a per-match range scan is back", len(body), d)
	}
}
