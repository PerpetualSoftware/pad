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

// ChatMaxReturnedSeq deliberately ignores last_seq and newest_seq: only
// messages in the returned page were displayed by this client.
func ChatMaxReturnedSeq(messages []struct {
	Seq int64 `json:"seq"`
}) int64 {
	var maxSeq int64
	for _, msg := range messages {
		if msg.Seq > maxSeq {
			maxSeq = msg.Seq
		}
	}
	return maxSeq
}
