package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4039: membership identities come only from the admitted credential.
func chatActor(w http.ResponseWriter, r *http.Request) (models.ConversationActor, bool) {
	if appContextFrom(r) != nil {
		writeError(w, 403, "forbidden", "Chat is not available to apps")
		return models.ConversationActor{}, false
	}
	u := currentUser(r)
	if u == nil {
		writeError(w, 401, "unauthorized", "Authentication required")
		return models.ConversationActor{}, false
	}
	if u.IsApp() {
		writeError(w, 403, "forbidden", "Chat is not available to apps")
		return models.ConversationActor{}, false
	}
	// BUG-4055: header names share attribution/presence normalization; see the ensure body check.
	key := agentNameFromRequest(r)
	if _, remote := remoteMCPCaller(r); remote && key == "" {
		writeError(w, 403, "agent_name_required", "Remote MCP chat requires an agent name")
		return models.ConversationActor{}, false
	}
	// TASK-4039 R1: cookie sessions always speak as the human.
	if !isBearerAuth(r) {
		key = ""
	}
	return models.ConversationActor{UserID: u.ID, AgentKey: key}, true
}
func writeChatError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrConversationNotFound):
		writeError(w, 404, "not_found", "Conversation not found")
	// TASK-4039: the store's stricter membership refusal shares the resolver's wire text.
	case errors.Is(err, store.ErrContextWorkspaceNotFound):
		writeError(w, 400, "validation_error", "context workspace not found")
	case errors.Is(err, store.ErrInvalidConversationInput):
		writeError(w, 400, "validation_error", err.Error())
	case errors.Is(err, store.ErrConversationForbidden):
		writeError(w, 403, "forbidden", "You can only delete your own messages")
	case errors.Is(err, store.ErrReplyTargetNotFound):
		writeError(w, 422, "reply_target_not_found", "Reply target not found")
	case errors.Is(err, store.ErrReplyTargetDeleted):
		writeError(w, 409, "reply_target_deleted", "Reply target was deleted")
	default:
		writeInternalError(w, err)
	}
}
func (s *Server) resolveChatConversation(w http.ResponseWriter, r *http.Request, actor models.ConversationActor) (*models.Conversation, bool) {
	n, err := strconv.Atoi(chi.URLParam(r, "n"))
	if err != nil || n <= 0 {
		writeChatError(w, store.ErrConversationNotFound)
		return nil, false
	}
	c, err := s.store.GetConversationByNumber(actor, n)
	if err != nil {
		writeChatError(w, err)
		return nil, false
	}
	return c, true
}
func (s *Server) handleListChatConversations(w http.ResponseWriter, r *http.Request) {
	actor, ok := chatActor(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListConversations(actor)
	if err != nil {
		writeChatError(w, err)
		return
	}
	out := make([]chatConversationSummary, 0, len(rows))
	total := 0
	for _, c := range rows {
		out = append(out, newChatSummary(c))
		total += c.Unread
	}
	writeJSON(w, 200, map[string]any{"conversations": out, "unread_total": total})
}
func (s *Server) handleEnsureChatConversation(w http.ResponseWriter, r *http.Request) {
	actor, ok := chatActor(w, r)
	if !ok {
		return
	}
	var in struct {
		AgentKey string `json:"agent_key"`
	}
	if err := decodeJSON(r, &in); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, 400, "validation_error", "Invalid JSON body")
		return
	}
	if actor.AgentKey != "" {
		if in.AgentKey != "" && in.AgentKey != actor.AgentKey {
			writeError(w, 400, "validation_error", "agent_key must match X-Pad-Agent")
			return
		}
		in.AgentKey = actor.AgentKey
	}
	// TASK-4039 r3: human-created DMs must be reachable through X-Pad-Agent.
	// BUG-4055 (ruling aea461b3): a body field is an explicit identifier and
	// must be refused rather than silently changed. The header is a self-declared
	// name shared with attribution and presence, so the normalizer truncates it.
	if actor.AgentKey == "" && normalizeAgentName(in.AgentKey) != in.AgentKey {
		writeError(w, 400, "validation_error", "agent_key must be a valid agent name")
		return
	}
	// TASK-4039 r2: reject malformed keys before spending a creation token.
	var invalidKey string
	switch {
	case in.AgentKey == "":
		invalidKey = "agent key is required"
	// BUG-4055 r1: shadowed, never reached. A human's key is refused above by the
	// r3 normalizeAgentName(in.AgentKey) != in.AgentKey gate, and an agent's key is
	// its already-normalised header. Kept so the bound holds if that gate moves.
	case utf8.RuneCountInString(in.AgentKey) > models.MaxAgentNameRunes:
		invalidKey = fmt.Sprintf("agent key is longer than %d characters", models.MaxAgentNameRunes)
	case !utf8.ValidString(in.AgentKey):
		invalidKey = "agent key is not valid UTF-8"
	case strings.ContainsFunc(in.AgentKey, unicode.IsControl):
		invalidKey = "agent key contains a control character"
	}
	if invalidKey != "" {
		writeError(w, 400, "validation_error", store.ErrInvalidConversationInput.Error()+": "+invalidKey)
		return
	}
	// TASK-4039 r2: use the human's inbox, even when the caller is an agent.
	rows, err := s.store.ListConversations(models.ConversationActor{UserID: actor.UserID})
	if err != nil {
		writeChatError(w, err)
		return
	}
	exists := false
	for _, row := range rows {
		if row.AgentKey == in.AgentKey {
			exists = true
			break
		}
	}
	// TASK-4039 r2: concurrent first creates can both spend a token; acceptable.
	if !exists && !s.allowChatDM(w, actor.UserID) {
		return
	}
	c, created, err := s.store.EnsureAgentDM(actor.UserID, in.AgentKey)
	if err != nil {
		writeChatError(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, map[string]any{"conversation": newChatConversation(*c, in.AgentKey), "created": created})
}
func (s *Server) handleGetChatConversation(w http.ResponseWriter, r *http.Request) {
	actor, ok := chatActor(w, r)
	if !ok {
		return
	}
	c, ok := s.resolveChatConversation(w, r, actor)
	if !ok {
		return
	}
	// TASK-4039: use the inbox for caller markers until polling warrants a store query.
	rows, err := s.store.ListConversations(actor)
	if err != nil {
		writeChatError(w, err)
		return
	}
	for _, row := range rows {
		if row.ID == c.ID {
			writeJSON(w, 200, chatConversationGet{chatConversationDetail: newChatDetail(row), DeliveredSeq: row.DeliveredSeq})
			return
		}
	}
	writeChatError(w, store.ErrConversationNotFound)
}
func (s *Server) handleListChatMessages(w http.ResponseWriter, r *http.Request) {
	actor, ok := chatActor(w, r)
	if !ok {
		return
	}
	c, ok := s.resolveChatConversation(w, r, actor)
	if !ok {
		return
	}
	var before, after *int64
	limit := 50
	for _, param := range []struct {
		name   string
		target **int64
	}{{"before_seq", &before}, {"after_seq", &after}} {
		if r.URL.Query().Has(param.name) {
			v, err := strconv.ParseInt(r.URL.Query().Get(param.name), 10, 64)
			if err != nil || v < 0 {
				writeError(w, 400, "validation_error", "Invalid "+param.name)
				return
			}
			*param.target = &v
		}
	}
	if r.URL.Query().Has("limit") {
		v, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || v < 0 {
			writeError(w, 400, "validation_error", "Invalid limit")
			return
		}
		limit = v
	}
	page, err := s.store.ListConversationMessages(actor, c.ID, before, after, limit)
	if err != nil {
		writeChatError(w, err)
		return
	}
	out := chatMessagesResponse{OldestSeq: page.OldestSeq, NewestSeq: page.NewestSeq, LastSeq: page.LastSeq, ReadSeq: page.ReadSeq, HasMore: page.HasMore}
	out.Messages, out.Refs, out.RefsTruncated = s.enrichChatMessages(r, actor, page.Messages)
	writeJSON(w, 200, out)
}
func (s *Server) handleSendChatMessage(w http.ResponseWriter, r *http.Request) {
	actor, ok := chatActor(w, r)
	if !ok {
		return
	}
	c, ok := s.resolveChatConversation(w, r, actor)
	if !ok {
		return
	}
	var in struct {
		Body             string `json:"body"`
		ReplyToSeq       *int64 `json:"reply_to_seq"`
		OpKey            string `json:"op_key"`
		ContextWorkspace string `json:"context_workspace"`
	}
	if err := decodeJSONWithLimit(r, &in, 128*1024); err != nil {
		writeError(w, 400, "validation_error", "Invalid JSON body (maximum 128 KiB)")
		return
	}

	// TASK-4039: cheap input refusals do not spend the agent send bucket.
	switch {
	case strings.TrimSpace(in.Body) == "":
		writeError(w, 400, "validation_error", "body is required")
		return
	case len(in.Body) > 64*1024:
		writeError(w, 400, "validation_error", "body is longer than 65536 bytes")
		return
	case len(in.OpKey) > 128:
		writeError(w, 400, "validation_error", "op_key is longer than 128 bytes")
		return
	case in.ReplyToSeq != nil && *in.ReplyToSeq < 1:
		writeError(w, 400, "validation_error", "reply_to_seq must be at least 1")
		return
	}
	// TASK-4039 r1: charge admitted members equally for both replay paths and sends.
	if !s.allowChatSend(w, c.ID, actor) {
		return
	}
	contextID := ""
	if in.ContextWorkspace != "" {
		access := s.AuthorizeCrossWorkspaceRead(r, in.ContextWorkspace, CrossWorkspaceWorkspaceOnlyScope())
		if !access.Allowed {
			if in.OpKey != "" {
				msg, found, err := s.store.ReplayConversationMessage(actor, c.ID, in.OpKey)
				if err != nil {
					writeChatError(w, err)
					return
				}
				if found {
					messages, refs, truncated := s.enrichChatMessages(r, actor, []models.ConversationMessage{*msg})
					writeJSON(w, 200, chatSendResponse{Message: messages[0], Replayed: true, Refs: refs, RefsTruncated: truncated})
					return
				}
			}
			writeError(w, 400, "validation_error", "context workspace not found")
			return
		}
		contextID = access.Workspace.ID
	}
	// TASK-4039 r1: source and authorship share the wire and in-process bearer signal.
	source := "web"
	if isBearerAuth(r) {
		source = "cli"
	}
	result, err := s.store.SendConversationMessage(models.ConversationSend{ConversationID: c.ID, Author: actor, Body: in.Body, ReplyToSeq: in.ReplyToSeq, OpKey: in.OpKey, Source: source, ContextWorkspaceID: contextID})
	if err != nil {
		writeChatError(w, err)
		return
	}
	if !result.Replayed {
		s.notifyChatMessage(c, result.Message, result.Recipients, false)
	}
	status := 201
	if result.Replayed {
		status = 200
	}
	messages, refs, truncated := s.enrichChatMessages(r, actor, []models.ConversationMessage{result.Message})
	writeJSON(w, status, chatSendResponse{Message: messages[0], Replayed: result.Replayed, Refs: refs, RefsTruncated: truncated})
}
func (s *Server) handleDeleteChatMessage(w http.ResponseWriter, r *http.Request) {
	actor, ok := chatActor(w, r)
	if !ok {
		return
	}
	c, ok := s.resolveChatConversation(w, r, actor)
	if !ok {
		return
	}
	seq, err := strconv.ParseInt(chi.URLParam(r, "seq"), 10, 64)
	if err != nil || seq <= 0 {
		writeChatError(w, store.ErrConversationNotFound)
		return
	}
	m, deleted, err := s.store.DeleteConversationMessage(actor, c.ID, seq)
	if err != nil {
		writeChatError(w, err)
		return
	}
	if deleted {
		s.notifyChatDeleted(c, *m, actor)
	}
	writeJSON(w, 200, map[string]any{"message": newChatMsg(actor, *m), "deleted": deleted})
}
func (s *Server) handleMarkChatRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := chatActor(w, r)
	if !ok {
		return
	}
	c, ok := s.resolveChatConversation(w, r, actor)
	if !ok {
		return
	}
	var in struct {
		Seq *int64 `json:"seq"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Seq == nil || *in.Seq < 0 {
		writeError(w, 400, "validation_error", "seq must be a nonnegative integer")
		return
	}
	seq, err := s.store.MarkConversationRead(actor, c.ID, *in.Seq)
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"read_seq": seq})
}
func (s *Server) handleMarkChatDelivered(w http.ResponseWriter, r *http.Request) {
	actor, ok := chatActor(w, r)
	if !ok {
		return
	}
	c, ok := s.resolveChatConversation(w, r, actor)
	if !ok {
		return
	}
	var in struct {
		Seq *int64 `json:"seq"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Seq == nil || *in.Seq < 0 {
		writeError(w, 400, "validation_error", "seq must be a nonnegative integer")
		return
	}
	seq, err := s.store.MarkConversationDelivered(actor, c.ID, *in.Seq)
	if err != nil {
		writeChatError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"delivered_seq": seq})
}
func (s *Server) handleChatUnread(w http.ResponseWriter, r *http.Request) {
	actor, ok := chatActor(w, r)
	if !ok {
		return
	}
	filter := r.URL.Query().Get("agent")
	if actor.AgentKey != "" && r.URL.Query().Has("agent") && filter != actor.AgentKey {
		writeError(w, 400, "validation_error", "agent must match X-Pad-Agent")
		return
	}
	rows, _, err := s.store.UnreadSummary(actor)
	if err != nil {
		writeChatError(w, err)
		return
	}
	out := make([]models.ConversationUnread, 0, len(rows))
	total := 0
	for _, row := range rows {
		if filter != "" && row.AgentKey != filter {
			continue
		}
		out = append(out, row)
		total += row.Unread
	}
	writeJSON(w, 200, map[string]any{"conversations": out, "total": total})
}
