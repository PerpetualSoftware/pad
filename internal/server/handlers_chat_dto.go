package server

import (
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4039: explicit wire types keep owner, author and context IDs private.
type chatConversation struct {
	ID        string    `json:"id"`
	Number    int       `json:"number"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	AgentKey  string    `json:"agent_key"`
	LastSeq   int64     `json:"last_seq"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type chatConversationDetail struct {
	chatConversation
	ReadSeq int64 `json:"read_seq"`
	Unread  int   `json:"unread"`
}
type chatConversationGet struct {
	chatConversationDetail
	DeliveredSeq int64 `json:"delivered_seq"`
}
type chatConversationSummary struct {
	chatConversationDetail
	LastMessage *chatLastMessage `json:"last_message,omitempty"`
}
type chatLastMessage struct {
	Seq            int64     `json:"seq"`
	AuthorAgentKey string    `json:"author_agent_key"`
	AuthorIsHuman  bool      `json:"author_is_human"`
	Excerpt        string    `json:"excerpt"`
	Deleted        bool      `json:"deleted"`
	CreatedAt      time.Time `json:"created_at"`
}
type chatMsg struct {
	ContextWorkspace *chatContextWorkspace     `json:"context_workspace,omitempty"`
	ID               string                    `json:"id"`
	ConversationID   string                    `json:"conversation_id"`
	Seq              int64                     `json:"seq"`
	AuthorAgentKey   string                    `json:"author_agent_key"`
	AuthorIsHuman    bool                      `json:"author_is_human"`
	Mine             bool                      `json:"mine"`
	Source           string                    `json:"source"`
	Body             string                    `json:"body"`
	ReplyToSeq       *int64                    `json:"reply_to_seq,omitempty"`
	ReplyTo          *models.ConversationQuote `json:"reply_to,omitempty"`
	CreatedAt        time.Time                 `json:"created_at"`
	Deleted          bool                      `json:"deleted"`
	DeletedAt        *time.Time                `json:"deleted_at,omitempty"`
}
type chatMessagesResponse struct {
	RefsTruncated bool      `json:"refs_truncated"`
	Refs          []chatRef `json:"refs"`
	Messages      []chatMsg `json:"messages"`
	OldestSeq     int64     `json:"oldest_seq"`
	NewestSeq     int64     `json:"newest_seq"`
	LastSeq       int64     `json:"last_seq"`
	ReadSeq       int64     `json:"read_seq"`
	HasMore       bool      `json:"has_more"`
}
type chatSendResponse struct {
	RefsTruncated bool      `json:"refs_truncated"`
	Refs          []chatRef `json:"refs"`
	Message       chatMsg   `json:"message"`
	Replayed      bool      `json:"replayed"`
}

func newChatConversation(c models.Conversation, agentKey string) chatConversation {
	return chatConversation{ID: c.ID, Number: c.Number, Kind: c.Kind, Title: c.Title, AgentKey: agentKey, LastSeq: c.LastSeq, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}
func newChatDetail(c models.ConversationSummary) chatConversationDetail {
	return chatConversationDetail{chatConversation: newChatConversation(c.Conversation, c.AgentKey), ReadSeq: c.ReadSeq, Unread: c.Unread}
}
func newChatSummary(c models.ConversationSummary) chatConversationSummary {
	out := chatConversationSummary{chatConversationDetail: newChatDetail(c)}
	if m := c.LastMessage; m != nil {
		out.LastMessage = &chatLastMessage{Seq: m.Seq, AuthorAgentKey: m.AuthorAgentKey, AuthorIsHuman: m.AuthorAgentKey == "", Excerpt: models.QuoteExcerpt(m.Body), Deleted: m.Deleted, CreatedAt: m.CreatedAt}
	}
	return out
}
func newChatMsg(actor models.ConversationActor, m models.ConversationMessage) chatMsg {
	return chatMsg{ID: m.ID, ConversationID: m.ConversationID, Seq: m.Seq, AuthorAgentKey: m.AuthorAgentKey, AuthorIsHuman: m.AuthorAgentKey == "", Mine: m.AuthorUserID == actor.UserID && m.AuthorAgentKey == actor.AgentKey, Source: m.Source, Body: m.Body, ReplyToSeq: m.ReplyToSeq, ReplyTo: m.ReplyTo, CreatedAt: m.CreatedAt, Deleted: m.Deleted, DeletedAt: m.DeletedAt}
}
