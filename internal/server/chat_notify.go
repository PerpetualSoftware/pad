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
	for _, target := range recipients {
		if target.AgentKey == "" || (target.UserID == m.AuthorUserID && target.AgentKey == m.AuthorAgentKey) {
			continue
		}
		s.publishWatchNotification(watchevents.Notification{
			Kind: watchevents.KindChat, ConversationID: c.ID, ConversationNumber: c.Number,
			MessageID: m.ID, MessageSeq: m.Seq, ReplyToSeq: m.ReplyToSeq, Excerpt: excerpt,
			AuthorUserID: m.AuthorUserID, AuthorAgentKey: m.AuthorAgentKey,
			TargetUserID: target.UserID, TargetAgentKey: target.AgentKey, ChatDeleted: deleted,
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
