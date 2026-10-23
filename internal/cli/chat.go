package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseChatTarget accepts an owner-scoped conversation number (C12 or 12)
// or a DM agent key (@rook). Chat never resolves against a workspace.
func ParseChatTarget(target string) (number int, agent string, err error) {
	if strings.HasPrefix(target, "@") && len(target) > 1 {
		return 0, target[1:], nil
	}
	digits := strings.TrimPrefix(target, "C")
	if digits != "" && strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		n, parseErr := strconv.Atoi(digits)
		if parseErr == nil && n > 0 {
			return n, "", nil
		}
	}
	return 0, "", fmt.Errorf("invalid chat target %q: use C<n>, <n>, or @<name>", target)
}

// ChatReadSeq returns the highest displayed sequence only when the page reaches
// the existing read marker. A high-water mark must not skip unseen messages.
// Conversation sequences are contiguous, including deleted-message tombstones.
func ChatReadSeq(messages []struct {
	Seq int64 `json:"seq"`
}, readSeq int64) int64 {
	if len(messages) == 0 {
		return 0
	}
	first, last := messages[0].Seq, messages[0].Seq
	for _, msg := range messages[1:] {
		if msg.Seq < first {
			first = msg.Seq
		}
		if msg.Seq > last {
			last = msg.Seq
		}
	}
	if first > readSeq && first-readSeq > 1 {
		return 0
	}
	return last
}
