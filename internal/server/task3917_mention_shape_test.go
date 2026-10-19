package server

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/links"
)

// TestMentionShapeParity pins links.ExtractMentions's name shape to
// ValidateUsername's, ignoring the reserved list (TASK-3917).
func TestMentionShapeParity(t *testing.T) {
	names := []string{
		"alice", "bob", "abc", "a-b", "a1b", "123", "a-b-c", "x9-9y",
		strings.Repeat("a", 39),
		"ab", "a", "", strings.Repeat("a", 40), "-abc", "abc-", "a--b", "ab--", "Alice",
	}
	for _, n := range names {
		valid := ValidateUsername(n) == nil
		if valid != (n != "" && n == strings.ToLower(n) && len(links.ExtractMentions("@"+n)) == 1 && links.ExtractMentions("@" + n)[0] == n) {
			t.Errorf("shape mismatch for %q: ValidateUsername valid=%v, mention=%v", n, valid, links.ExtractMentions("@"+n))
		}
	}
}
