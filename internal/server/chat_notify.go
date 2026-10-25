package server

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
	"log/slog"
)

// TASK-4041, DOC-4036: publish after the store commits, with no full body;
// a bounded excerpt (at most 100 runes, first line) rides on the event.
func (s *Server) notifyChatMessage(c *models.Conversation, m models.ConversationMessage, recipients []models.ConversationActor, deleted bool) {
	excerpt := models.QuoteExcerpt(m.Body)
	if deleted {
		excerpt = ""
	}
	// TASK-4044: a human member gets a ref-only inbox notice, delivered only
	// to that user's own cookie streams (chat_inbox). It never carries the
	// excerpt, the reply seq or an agent key: the web refetches through the
	// access-checked API. The agent notices below keep theirs, the monitor's
	// line depends on them.
	humans := map[string]bool{}
	var humanOrder []string
	addHuman := func(userID string) {
		if userID != "" && !humans[userID] {
			humans[userID] = true
			humanOrder = append(humanOrder, userID)
		}
	}
	for _, target := range recipients {
		if target.AgentKey == "" {
			addHuman(target.UserID)
			continue
		}
		if target.UserID == m.AuthorUserID && target.AgentKey == m.AuthorAgentKey {
			continue
		}
		s.publishWatchNotification(watchevents.Notification{
			Kind: watchevents.KindChat, ConversationID: c.ID, ConversationNumber: c.Number,
			MessageID: m.ID, MessageSeq: m.Seq, ReplyToSeq: m.ReplyToSeq, Excerpt: excerpt,
			AuthorUserID: m.AuthorUserID, AuthorAgentKey: m.AuthorAgentKey,
			TargetUserID: target.UserID, TargetAgentKey: target.AgentKey, ChatDeleted: deleted,
		})
	}
	// A human author's send leaves the author pair out of recipients; their
	// other tabs still need the ref. The delete path includes the author, so
	// the set dedupes it.
	if m.AuthorAgentKey == "" {
		addHuman(m.AuthorUserID)
	}
	for _, userID := range humanOrder {
		s.publishWatchNotification(watchevents.Notification{
			Kind: watchevents.KindChat, ConversationID: c.ID, ConversationNumber: c.Number,
			MessageID: m.ID, MessageSeq: m.Seq,
			AuthorUserID: m.AuthorUserID, AuthorAgentKey: m.AuthorAgentKey,
			TargetUserID: userID, ChatDeleted: deleted,
		})
	}
}

func (s *Server) notifyChatDeleted(c *models.Conversation, m models.ConversationMessage, actor models.ConversationActor) {
	recipients, err := s.store.ConversationMemberActors(actor, c.ID)
	if err != nil {
		slog.Warn("chat retraction recipients unavailable", "error", err)
		return
	}
	s.notifyChatMessage(c, m, recipients, true)
}
