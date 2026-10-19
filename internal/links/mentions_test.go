package links

import (
	"reflect"
	"strings"
	"testing"
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
