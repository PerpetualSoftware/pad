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
	}
	for _, c := range cases {
		got := ExtractMentions(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ExtractMentions(%q) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
