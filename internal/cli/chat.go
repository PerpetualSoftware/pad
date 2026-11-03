package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ParseChatTarget accepts an owner-scoped conversation number (C12 or 12)
// or a DM agent key (@name). Chat never resolves against a workspace.
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

// CheckAgentChatTarget refuses, before any request, an @<name> send target
// that names a DM other than the calling agent's own. An agent has one chat,
// with its owner, and the server enforces that on send
// (handleEnsureChatConversation: agent_key must match the normalised
// X-Pad-Agent); this only replaces that refusal with a message naming the
// right target. Callers run it for send only: show, read and delete keep their
// truthful "not found". It compares exactly as the server does: the caller is
// normalised, the typed name is not, and case is not folded. A human (an empty
// normalised caller) and C<n> / <n> targets pass. It echoes only the caller's
// own name and the target typed, never another agent's.
func CheckAgentChatTarget(callerAgent, target string) error {
	caller := models.NormalizeAgentName(callerAgent)
	if caller == "" {
		return nil
	}
	_, agent, err := ParseChatTarget(target)
	if err != nil || agent == "" || agent == caller {
		return nil
	}
	return fmt.Errorf("as agent %q you have one chat, with your owner: use @%s or C<n> (pad chat list); @%s is another agent's DM", caller, caller, agent)
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
