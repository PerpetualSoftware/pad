package links

import (
	"regexp"
	"sort"
	"strings"
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

// MaxMentionsPerComment caps the distinct names the store resolves for one
// comment, bounding the IN-list a hostile body could grow. Names past the cap
// (in sorted order) are not indexed. ExtractMentions itself is uncapped.
const MaxMentionsPerComment = 50

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

// ExtractMentions returns the usernames a comment body @-mentions: lowercased,
// deduplicated and sorted. Mentions inside fenced or inline code and the
// domain half of an email address are skipped. A trailing hyphen on the run
// ("@alice-") is punctuation and is trimmed; any other malformed run is
// dropped. A match preceded by "/" or "\" (a path or URL) is skipped.
// Known limits, by decision (TASK-3917 r1): "end.@alice" is read as an email
// and skipped; usernames under 3 characters (legacy) never match; HTML such as
// <code>@alice</code> is not parsed as code. It only parses: resolving a name to a user is the store's job.
// (TASK-3917, IDEA-3906)
func ExtractMentions(content string) []string {
	if content == "" || !strings.Contains(content, "@") {
		return nil
	}
	fenced := fencedCodeRanges(content)
	inline := inlineCodeRanges(content, fenced)

	seen := map[string]bool{}
	var out []string
	for _, m := range mentionPattern.FindAllStringSubmatchIndex(content, -1) {
		at := m[0]
		if isInRanges(at, fenced) || isInRanges(at, inline) {
			continue
		}
		if at > 0 && (emailLocalByte(content[at-1]) || content[at-1] == '/' || content[at-1] == '\\') {
			continue
		}
		// A run followed by "_", by "." plus an ASCII letter or digit (a
		// domain or file name), or by a non-ASCII byte is not a handle.
		if end := m[1]; end < len(content) {
			c := content[end]
			if c == '_' || c >= 0x80 {
				continue
			}
			if c == '.' && end+1 < len(content) && isASCIIAlnum(content[end+1]) {
				continue
			}
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
