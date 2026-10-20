package links

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// mentionPattern finds an "@" followed by a run of name characters. The shape
// check (length, hyphens) is applied to the run afterwards so an over-long run
// is rejected whole rather than truncated into a different user's name.
var mentionPattern = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9-]*)`)

// MentionMinLen and MentionMaxLen bound a mentionable username. They mirror
// server/username.go::ValidateUsername (3-39 chars, lowercase letters, digits
// and hyphens, no leading, trailing or doubled hyphen). The server package
// imports this one, not the reverse, so the shape is duplicated here and
// pinned by TestMentionShapeParity in the server package (TASK-3917). The
// reserved-name list is deliberately not copied: a reserved name has no user,
// so it never resolves.
const (
	MentionMinLen = 3
	MentionMaxLen = 39
)

// MaxMentionsPerComment caps the RESOLVED mentions the store indexes for one
// comment. The cap applies after resolution, in sorted name order, so names
// that resolve to no user cannot evict real ones. ExtractMentions is uncapped.
const MaxMentionsPerComment = 50

// MaxMentionCandidates bounds the parsed names the store will even try to
// resolve for one comment (in sorted order); the store resolves them in
// chunks of MentionResolveChunk. Names past it are not indexed.
const (
	MaxMentionCandidates = 500
	MentionResolveChunk  = 50
)

// isMentionShape reports whether name (already lowercased) has the shape of a
// valid username.
func isMentionShape(name string) bool {
	if len(name) < MentionMinLen || len(name) > MentionMaxLen {
		return false
	}
	if name[0] == '-' || name[len(name)-1] == '-' || strings.Contains(name, "--") {
		return false
	}
	return true
}

func isASCIIAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// emailLocalByte reports whether b can sit in an email local part, so an "@"
// right after it is an address and not a mention.
func emailLocalByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return strings.IndexByte("._%+-", b) >= 0
}

// urlScan answers "is the non-space run ending at this @ a URL" in O(1) per
// match. Matches arrive in increasing order, so advance walks each byte once
// and an ExtractMentions call stays linear in the body (TASK-3917 r3: a
// backward scan per match made "/@a" repeated quadratic).
type urlScan struct {
	pos      int // bytes before pos have been scanned
	runStart int // start of the current non-space run
	scheme   int // start of the last "://" seen, or -1
}

func (u *urlScan) advance(content string, at int) {
	for ; u.pos < at; u.pos++ {
		switch {
		case isSpaceByte(content[u.pos]):
			u.runStart = u.pos + 1
		case content[u.pos] == ':' && strings.HasPrefix(content[u.pos:], "://"):
			u.scheme = u.pos
		}
	}
}

// inURL reports whether the non-space run before the "@" at content[at]
// contains "://" or starts with "www.". advance(content, at) must have run.
func (u *urlScan) inURL(content string, at int) bool {
	return (u.scheme >= u.runStart && u.scheme+3 <= at) ||
		strings.HasPrefix(content[u.runStart:at], "www.")
}

// rangeCursor answers "is pos inside one of these ranges" for positions that
// arrive in increasing order, walking the sorted, disjoint ranges once per
// call rather than once per match (TASK-3917 r4: a scan of every range per
// match made a body of many code spans and then many mentions quadratic).
// ExtractWikiLinks uses it too (BUG-3968).
type rangeCursor struct {
	ranges [][2]int
	i      int
}

func (c *rangeCursor) contains(pos int) bool {
	for c.i < len(c.ranges) && c.ranges[c.i][1] <= pos {
		c.i++
	}
	return c.i < len(c.ranges) && pos >= c.ranges[c.i][0]
}

// badMentionPrefix reports whether the byte before the "@" at content[at]
// makes it a non-mention (an email local part, a path, a URL).
func badMentionPrefix(content string, at int, u *urlScan) bool {
	c := content[at-1]
	switch c {
	case '\\':
		return true
	case '/':
		return u.inURL(content, at)
	case '_':
		// Italics ("_@alice_"): the "_" opens the word.
		return !(at-1 == 0 || isSpaceByte(content[at-2]))
	}
	return emailLocalByte(c)
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// badMentionFollower reports whether what follows a run ending at content[end]
// shows it is not a handle (part of a longer word, a domain or a file name).
func badMentionFollower(content string, end int) bool {
	if end >= len(content) {
		return false
	}
	c := content[end]
	switch {
	case c >= 0x80:
		r, _ := utf8.DecodeRuneInString(content[end:])
		return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
	case c == '_':
		return end+1 < len(content) && isASCIIAlnum(content[end+1])
	case c == '.':
		return end+1 < len(content) && isASCIIAlnum(content[end+1])
	}
	return false
}

// ExtractMentions returns the usernames a comment body @-mentions: lowercased,
// deduplicated and sorted. Mentions inside fenced or inline code and the
// domain half of an email address are skipped. A trailing hyphen on the run
// ("@alice-") is punctuation and is trimmed; any other malformed run is
// dropped. A match preceded by "\", or by "/" inside a URL (the non-space run
// before the "@" contains "://" or starts with "www."), is skipped; any other
// "/" is a separator ("cc @alice/@bob" yields both). A "_" prefix is allowed
// only at the start or after whitespace (italics: "_@alice_").
// A name ends at the first rune that cannot continue a handle: a following
// ASCII letter or digit is part of the run, a following non-ASCII letter,
// digit or combining mark rejects the run, any other rune (NBSP, curly
// apostrophe, ellipsis, dash, emoji, fullwidth punctuation) terminates it. A
// following "_" rejects only when an ASCII letter or digit follows it
// ("@alice_bob" no, "@alice_" yes); "." plus an ASCII letter or digit rejects.
// Known limits, by decision (TASK-3917 r1, r2): "end.@alice" is read as an
// email and skipped; "@alice.Thanks" (no space after the period) is rejected,
// indistinguishable from "@alice.smith"; "-@alice" is skipped; "@alice" directly
// followed by a CJK (or any non-ASCII) letter is not indexed, since that rule
// keeps "@josé" from indexing "jos"; usernames under
// 3 characters (legacy) never match; HTML such as <code>@alice</code> is not
// parsed as code. It only parses: resolving a name to a user is the store's job.
// (TASK-3917, IDEA-3906)
func ExtractMentions(content string) []string {
	if content == "" || !strings.Contains(content, "@") {
		return nil
	}
	fenced := fencedCodeRanges(content)
	inline := inlineCodeRanges(content, fenced)

	seen := map[string]bool{}
	var out []string
	u := urlScan{scheme: -1}
	fc, ic := rangeCursor{ranges: fenced}, rangeCursor{ranges: inline}
	for _, m := range mentionPattern.FindAllStringSubmatchIndex(content, -1) {
		at := m[0]
		u.advance(content, at)
		if fc.contains(at) || ic.contains(at) {
			continue
		}
		if at > 0 && badMentionPrefix(content, at, &u) {
			continue
		}
		if badMentionFollower(content, m[1]) {
			continue
		}
		name := strings.ToLower(strings.TrimRight(content[m[2]:m[3]], "-"))
		if !isMentionShape(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
