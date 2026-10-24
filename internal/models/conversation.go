package models

import (
	"strings"
	"time"
	"unicode"
)

// MaxAgentNameRunes bounds self-declared agent names and chat agent keys (BUG-4055).
const MaxAgentNameRunes = 64

// ConversationExcerptRunes caps a quoted message's excerpt (TASK-4038).
const ConversationExcerptRunes = 100

// ConversationActor is a member identity: a user, or one of that user's
// agents. AgentKey "" is the human.
type ConversationActor struct {
	UserID   string
	AgentKey string
}

// Conversation is a chat thread. Global (no workspace), numbered per owner.
type Conversation struct {
	ID          string    `json:"id"`
	OwnerUserID string    `json:"owner_user_id"`
	Number      int       `json:"number"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	DMKey       string    `json:"dm_key,omitempty"`
	LastSeq     int64     `json:"last_seq"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ConversationSummary is one inbox row.
type ConversationSummary struct {
	Conversation
	AgentKey     string               `json:"agent_key"`
	ReadSeq      int64                `json:"read_seq"`
	DeliveredSeq int64                `json:"delivered_seq"`
	Unread       int                  `json:"unread"`
	LastMessage  *ConversationMessage `json:"last_message,omitempty"`
}

// ConversationQuote is the preview of the message a reply quotes. Never the
// full body.
type ConversationQuote struct {
	Seq            int64  `json:"seq"`
	AuthorAgentKey string `json:"author_agent_key"`
	AuthorIsHuman  bool   `json:"author_is_human"`
	Excerpt        string `json:"excerpt"`
	Deleted        bool   `json:"deleted"`
}

// ConversationMessage is one message. A deleted message keeps its row and seq
// with an empty Body.
type ConversationMessage struct {
	ID                 string             `json:"id"`
	ConversationID     string             `json:"conversation_id"`
	Seq                int64              `json:"seq"`
	AuthorUserID       string             `json:"author_user_id"`
	AuthorAgentKey     string             `json:"author_agent_key"`
	Source             string             `json:"source"`
	Body               string             `json:"body"`
	ReplyToSeq         *int64             `json:"reply_to_seq,omitempty"`
	ReplyTo            *ConversationQuote `json:"reply_to,omitempty"`
	ContextWorkspaceID *string            `json:"context_workspace_id,omitempty"`
	CreatedAt          time.Time          `json:"created_at"`
	Deleted            bool               `json:"deleted"`
	DeletedAt          *time.Time         `json:"deleted_at,omitempty"`
}

// ConversationSend is a send request.
type ConversationSend struct {
	ConversationID     string
	Author             ConversationActor
	Body               string
	Source             string
	ReplyToSeq         *int64
	ContextWorkspaceID string
	OpKey              string
}

// ConversationSendResult is a send's outcome. Replayed means the op_key had
// already been used and Message is the stored message, not a new one.
// TASK-4038 r6: a send leaves both read and delivered markers unchanged.
// TASK-4038: Recipients contains every OTHER member identity, including the
// human member, because the web also needs a ref-only nudge. TASK-4041 filters
// to agent members for monitor delivery. A Replayed result has no Recipients;
// the caller must not notify on a replay.
type ConversationSendResult struct {
	Message    ConversationMessage
	Replayed   bool
	Recipients []ConversationActor
}

// ConversationPage is a window of messages in ascending seq order, tombstones
// included.
// TASK-4038: HasMore applies only to the requested direction. For a non-empty
// page, older messages exist iff OldestSeq > 1; newer messages exist iff
// NewestSeq < LastSeq, which is why LastSeq is included on the page.
type ConversationPage struct {
	Messages  []ConversationMessage `json:"messages"`
	OldestSeq int64                 `json:"oldest_seq"`
	NewestSeq int64                 `json:"newest_seq"`
	LastSeq   int64                 `json:"last_seq"`
	ReadSeq   int64                 `json:"read_seq"`
	HasMore   bool                  `json:"has_more"`
}

// ConversationUnread is one membership's unread state.
type ConversationUnread struct {
	ConversationID string `json:"conversation_id"`
	Number         int    `json:"number"`
	AgentKey       string `json:"agent_key"`
	Unread         int    `json:"unread"`
	LastSeq        int64  `json:"last_seq"`
	ReadSeq        int64  `json:"read_seq"`
	DeliveredSeq   int64  `json:"delivered_seq"`
}

// AgentDMKey is the dm_key of an owner's direct conversation with an agent.
// TASK-4038 r3: owner user ids are store-generated UUIDs (store.go newID) with no ':', so agent:<owner>:<agent_key> splits unambiguously at the second ':' even when agent_key contains ':'.
func AgentDMKey(owner, agentKey string) string {
	return "agent:" + owner + ":" + agentKey
}

// QuoteExcerpt is the first non-empty line of body with whitespace collapsed,
// cut so the result, "..." included, is at most ConversationExcerptRunes
// runes. "" for an empty body.
func QuoteExcerpt(body string) string {
	line := ""
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	line = strings.Join(strings.FieldsFunc(line, unicode.IsSpace), " ")
	r := []rune(line)
	if len(r) <= ConversationExcerptRunes {
		return line
	}
	return string(r[:ConversationExcerptRunes-3]) + "..."
}
