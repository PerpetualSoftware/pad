package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Chat conversations (TASK-4038, PLAN-4037 phase 2a). Every method that takes
// a ConversationActor enforces membership itself and answers a non-member
// with ErrConversationNotFound, so a caller cannot forget the check and a
// conversation never confirms its own existence to an outsider.

var (
	ErrConversationNotFound     = errors.New("conversation not found")
	ErrConversationForbidden    = errors.New("not allowed on this conversation message")
	ErrReplyTargetNotFound      = errors.New("reply target not found")
	ErrReplyTargetDeleted       = errors.New("reply target was deleted")
	ErrInvalidConversationInput = errors.New("invalid conversation input")
	// TASK-4039 r1: retain the input-error identity and text for context refusals.
	ErrContextWorkspaceNotFound = fmt.Errorf("%w: context workspace not found", ErrInvalidConversationInput)
)

const (
	maxConversationIDBytes   = 128
	maxConversationBodyBytes = 64 * 1024
	maxConversationSource    = 32
	maxConversationOpKey     = 128
	maxConversationContextID = 128

	defaultConversationPage = 50
	maxConversationPage     = 200
)

// TASK-4038: like decisionTimeFormat in decisions.go and idemTimeFormat in
// idempotency.go, keep nanoseconds fixed-width so TEXT timestamps sort by time.
const chatTimeFormat = "2006-01-02T15:04:05.000000000Z"

func chatTime(t time.Time) string { return t.UTC().Format(chatTimeFormat) }

func chatNow() string { return chatTime(time.Now()) }

func invalidConversation(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidConversationInput, fmt.Sprintf(format, args...))
}

// validateConversationAgentKey checks an agent key. "" (the human) is valid
// only when allowEmpty. The key is stored exactly as given.
func validateConversationAgentKey(key string, allowEmpty bool) error {
	if key == "" {
		if allowEmpty {
			return nil
		}
		return invalidConversation("agent key is required")
	}
	if !utf8.ValidString(key) {
		return invalidConversation("agent key is not valid UTF-8")
	}
	if utf8.RuneCountInString(key) > models.MaxAgentNameRunes {
		return invalidConversation("agent key is longer than %d characters", models.MaxAgentNameRunes)
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return invalidConversation("agent key contains a control character")
		}
	}
	return nil
}

// TASK-4038: reject malformed actor identities before they reach either SQL driver.
func validateConversationActor(actor models.ConversationActor) error {
	if actor.UserID == "" {
		return invalidConversation("user id is required")
	}
	if strings.ContainsRune(actor.UserID, 0) || !utf8.ValidString(actor.UserID) {
		return invalidConversation("user id must be valid UTF-8 without NUL bytes")
	}
	return validateConversationAgentKey(actor.AgentKey, true)
}

// TASK-4038 r6: malformed ids name no conversation; reject them before SQL.
func validateConversationID(id string) error {
	if id == "" || len(id) > maxConversationIDBytes || strings.ContainsRune(id, 0) || !utf8.ValidString(id) {
		return ErrConversationNotFound
	}
	return nil
}

// EnsureAgentDM returns the owner's direct conversation with the agent,
// creating it (and its two member rows) on first use. created reports whether
// this call made it. Idempotent on dm_key.
func (s *Store) EnsureAgentDM(ownerUserID, agentKey string) (*models.Conversation, bool, error) {
	// TASK-4038 r4: the same identity check every actor method runs, so a
	// malformed owner id is an input error rather than a driver error.
	if err := validateConversationActor(models.ConversationActor{UserID: ownerUserID}); err != nil {
		return nil, false, err
	}
	if err := validateConversationAgentKey(agentKey, false); err != nil {
		return nil, false, err
	}
	dmKey := models.AgentDMKey(ownerUserID, agentKey)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin ensure dm: %w", err)
	}
	defer tx.Rollback()

	// SQLite's immediate transaction already serialises writers; Postgres
	// needs the per-owner lock so two first creates cannot pick one number.
	if s.dialect.Driver() == DriverPostgres {
		if _, err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext('pad:conversation-number:' || $1))", ownerUserID); err != nil {
			return nil, false, fmt.Errorf("acquire conversation number lock: %w", err)
		}
	}

	c, err := scanConversation(tx.QueryRow(s.q(conversationSelect+` WHERE c.dm_key = ?`), dmKey))
	if err == nil {
		return c, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("lookup dm: %w", err)
	}

	id, ts := newID(), chatNow()
	if _, err := tx.Exec(s.q(`
		INSERT INTO conversations (id, owner_user_id, number, kind, title, dm_key, last_seq, created_at, updated_at)
		VALUES (?, ?, (SELECT COALESCE(MAX(number), 0) + 1 FROM conversations WHERE owner_user_id = ?), 'direct', '', ?, 0, ?, ?)
	`), id, ownerUserID, ownerUserID, dmKey, ts, ts); err != nil {
		return nil, false, fmt.Errorf("create dm: %w", err)
	}
	for _, key := range []string{"", agentKey} {
		if err := s.addConversationMemberTx(tx, id, ownerUserID, key, ts); err != nil {
			return nil, false, err
		}
	}
	c, err = scanConversation(tx.QueryRow(s.q(conversationSelect+` WHERE c.id = ?`), id))
	if err != nil {
		return nil, false, fmt.Errorf("reload dm: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit ensure dm: %w", err)
	}
	return c, true, nil
}

// addConversationMemberTx is the only writer of member rows in 2a; there is
// deliberately no public add or remove until threads and groups exist.
func (s *Store) addConversationMemberTx(tx *sql.Tx, convID, userID, agentKey, ts string) error {
	if _, err := tx.Exec(s.q(`
		INSERT INTO conversation_members (conversation_id, user_id, agent_key, read_seq, delivered_seq, added_at)
		VALUES (?, ?, ?, 0, 0, ?)
	`), convID, userID, agentKey, ts); err != nil {
		return fmt.Errorf("add conversation member: %w", err)
	}
	return nil
}

const conversationSelect = `SELECT c.id, c.owner_user_id, c.number, c.kind, c.title, c.dm_key, c.last_seq, c.created_at, c.updated_at FROM conversations c`

// conversationMemberOf restricts a conversationSelect to an actor's
// conversations. Its two placeholders are user id and agent key.
const conversationMemberOf = `EXISTS (SELECT 1 FROM conversation_members m WHERE m.conversation_id = c.id AND m.user_id = ? AND m.agent_key = ?)`

func scanConversation(row rowScanner) (*models.Conversation, error) {
	var c models.Conversation
	var dmKey sql.NullString
	var created, updated string
	if err := row.Scan(&c.ID, &c.OwnerUserID, &c.Number, &c.Kind, &c.Title, &dmKey, &c.LastSeq, &created, &updated); err != nil {
		return nil, err
	}
	c.DMKey = dmKey.String
	c.CreatedAt, c.UpdatedAt = parseTime(created), parseTime(updated)
	return &c, nil
}

// GetConversation returns a conversation the actor is a member of.
func (s *Store) GetConversation(actor models.ConversationActor, id string) (*models.Conversation, error) {
	if err := validateConversationID(id); err != nil {
		return nil, err
	}
	if err := validateConversationActor(actor); err != nil {
		return nil, err
	}
	c, err := scanConversation(s.db.QueryRow(s.q(conversationSelect+` WHERE c.id = ? AND `+conversationMemberOf), id, actor.UserID, actor.AgentKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation: %w", err)
	}
	return c, nil
}

// GetConversationByNumber resolves a number in the actor's own numbering
// (numbers are per owner; 2a conversations are all the actor's own).
func (s *Store) GetConversationByNumber(actor models.ConversationActor, number int) (*models.Conversation, error) {
	if err := validateConversationActor(actor); err != nil {
		return nil, err
	}
	c, err := scanConversation(s.db.QueryRow(s.q(conversationSelect+` WHERE c.owner_user_id = ? AND c.number = ? AND `+conversationMemberOf), actor.UserID, number, actor.UserID, actor.AgentKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get conversation by number: %w", err)
	}
	return c, nil
}

// SendConversationMessage appends a message and returns it. The seq comes from
// conversations.last_seq under that row's lock, in the same statement that
// checks the author is a member. A send whose op_key was already used by the
// same author in the same conversation is rolled back (no seq is spent) and
// returns the stored message with Replayed set, whatever body it carried.
// TASK-4038 r6: sends leave both read and delivered markers unchanged.
func (s *Store) SendConversationMessage(in models.ConversationSend) (*models.ConversationSendResult, error) {
	if err := validateConversationID(in.ConversationID); err != nil {
		return nil, err
	}
	if err := validateConversationActor(in.Author); err != nil {
		return nil, err
	}
	for _, field := range []struct{ name, value string }{
		{"body", in.Body},
		{"source", in.Source},
		{"op_key", in.OpKey},
		{"context workspace id", in.ContextWorkspaceID},
	} {
		if strings.ContainsRune(field.value, 0) || !utf8.ValidString(field.value) {
			return nil, invalidConversation("%s must be valid UTF-8 without NUL bytes", field.name)
		}
	}
	if in.Body == "" || strings.TrimSpace(in.Body) == "" {
		return nil, invalidConversation("body is required")
	}
	if len(in.Body) > maxConversationBodyBytes {
		return nil, invalidConversation("body is longer than %d bytes", maxConversationBodyBytes)
	}
	if len(in.Source) > maxConversationSource {
		return nil, invalidConversation("source is longer than %d bytes", maxConversationSource)
	}
	if len(in.OpKey) > maxConversationOpKey {
		return nil, invalidConversation("op_key is longer than %d bytes", maxConversationOpKey)
	}
	if len(in.ContextWorkspaceID) > maxConversationContextID {
		return nil, invalidConversation("context workspace id is longer than %d bytes", maxConversationContextID)
	}
	if in.ReplyToSeq != nil && *in.ReplyToSeq < 1 {
		return nil, invalidConversation("reply_to_seq must be at least 1")
	}

	var res *models.ConversationSendResult
	// One transaction per call, no effect outside it before the commit.
	err := s.retryOnDeadlock("conversation_send", func() error {
		var err error
		res, err = s.sendConversationMessageOnce(in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// TASK-4038 r6: the literal nonempty op_key term lets the partial index be used.
const conversationOpKeyLookup = `SELECT seq FROM conversation_messages
	WHERE conversation_id = ? AND author_user_id = ? AND author_agent_key = ? AND op_key = ? AND op_key <> ''`

const conversationOpKeyLookupWithMembership = conversationOpKeyLookup + `
	AND EXISTS (SELECT 1 FROM conversation_members cm
		WHERE cm.conversation_id = conversation_messages.conversation_id AND cm.user_id = ? AND cm.agent_key = ?)`

// ReplayConversationMessage returns a current conversation member's own message
// for opKey. TASK-4039 r1: an identical HTTP retry can lose context workspace
// access before reaching SendConversationMessage. This read-only lookup takes no
// row lock and opens no write transaction. Empty or over-long keys are not found.
func (s *Store) ReplayConversationMessage(actor models.ConversationActor, convID, opKey string) (*models.ConversationMessage, bool, error) {
	if err := validateConversationID(convID); err != nil {
		return nil, false, err
	}
	if err := validateConversationActor(actor); err != nil {
		return nil, false, err
	}
	if opKey == "" || len(opKey) > maxConversationOpKey {
		return nil, false, nil
	}
	var seq int64
	err := s.db.QueryRow(s.q(conversationOpKeyLookupWithMembership), convID, actor.UserID, actor.AgentKey, opKey, actor.UserID, actor.AgentKey).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup op_key: %w", err)
	}
	msg, err := s.conversationMessageBySeq(s.db, convID, seq)
	if err != nil {
		return nil, false, err
	}
	return msg, true, nil
}

func (s *Store) sendConversationMessageOnce(in models.ConversationSend) (*models.ConversationSendResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin send: %w", err)
	}
	defer tx.Rollback()

	if in.ContextWorkspaceID != "" {
		// TASK-4038: join the author's membership and lock only the workspace
		// before chat rows, matching PurgeWorkspaceData's lock order.
		// Otherwise the INSERT's FK lock can deadlock with the purge's ON DELETE SET NULL.
		query := `SELECT 1 FROM workspaces w
			JOIN workspace_members wm ON wm.workspace_id = w.id AND wm.user_id = ?
			WHERE w.id = ? AND w.deleted_at IS NULL`
		if s.dialect.Driver() == DriverPostgres {
			query += ` FOR KEY SHARE OF w`
		}
		var exists int
		if err := tx.QueryRow(s.q(query), in.Author.UserID, in.ContextWorkspaceID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			// TASK-4038 r5: a committed send still replays after context loss.
			// Check conversation membership without row locks so the workspace-
			// then-conversation lock order stays intact; this path writes nothing.
			if in.OpKey != "" {
				var prior int64
				err := tx.QueryRow(s.q(conversationOpKeyLookupWithMembership), in.ConversationID, in.Author.UserID, in.Author.AgentKey, in.OpKey, in.Author.UserID, in.Author.AgentKey).Scan(&prior)
				if err == nil {
					msg, err := s.conversationMessageBySeq(tx, in.ConversationID, prior)
					if err != nil {
						return nil, err
					}
					return &models.ConversationSendResult{Message: *msg, Replayed: true}, nil
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return nil, fmt.Errorf("lookup op_key: %w", err)
				}
			}
			return nil, ErrContextWorkspaceNotFound
		} else if err != nil {
			return nil, fmt.Errorf("check context workspace: %w", err)
		}
	}

	ts := chatNow()
	var seq int64
	err = tx.QueryRow(s.q(`
		UPDATE conversations SET last_seq = last_seq + 1, updated_at = ?
		WHERE id = ? AND EXISTS (
			SELECT 1 FROM conversation_members m
			WHERE m.conversation_id = conversations.id AND m.user_id = ? AND m.agent_key = ?)
		RETURNING last_seq
	`), ts, in.ConversationID, in.Author.UserID, in.Author.AgentKey).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("take seq: %w", err)
	}

	if in.OpKey != "" {
		var prior int64
		err := tx.QueryRow(s.q(conversationOpKeyLookup), in.ConversationID, in.Author.UserID, in.Author.AgentKey, in.OpKey).Scan(&prior)
		if err == nil {
			msg, err := s.conversationMessageBySeq(tx, in.ConversationID, prior)
			if err != nil {
				return nil, err
			}
			// Returning rolls the transaction back: the seq bump is undone.
			return &models.ConversationSendResult{Message: *msg, Replayed: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("lookup op_key: %w", err)
		}
	}

	if in.ReplyToSeq != nil {
		var deleted sql.NullString
		err := tx.QueryRow(s.q(`SELECT deleted_at FROM conversation_messages WHERE conversation_id = ? AND seq = ?`),
			in.ConversationID, *in.ReplyToSeq).Scan(&deleted)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrReplyTargetNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("lookup reply target: %w", err)
		}
		if deleted.Valid {
			return nil, ErrReplyTargetDeleted
		}
	}

	var ctxWS any
	if in.ContextWorkspaceID != "" {
		ctxWS = in.ContextWorkspaceID
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO conversation_messages
			(id, conversation_id, seq, author_user_id, author_agent_key, source, body, reply_to_seq, context_workspace_id, op_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), newID(), in.ConversationID, seq, in.Author.UserID, in.Author.AgentKey, in.Source, in.Body,
		in.ReplyToSeq, ctxWS, in.OpKey, ts); err != nil {
		return nil, fmt.Errorf("insert message: %w", err)
	}

	// TASK-4038 r6: leave both markers alone: high-water marks would cover
	// the other side's unseen messages. "Replying marks read" is the client's
	// job via MarkConversationRead with the seq it actually displayed;
	// delivered_seq moves only through MarkConversationDelivered.

	msg, err := s.conversationMessageBySeq(tx, in.ConversationID, seq)
	if err != nil {
		return nil, err
	}

	rows, err := tx.Query(s.q(`
		SELECT user_id, agent_key FROM conversation_members
		WHERE conversation_id = ? AND NOT (user_id = ? AND agent_key = ?)
		ORDER BY user_id, agent_key
	`), in.ConversationID, in.Author.UserID, in.Author.AgentKey)
	if err != nil {
		return nil, fmt.Errorf("list recipients: %w", err)
	}
	var recipients []models.ConversationActor
	for rows.Next() {
		var a models.ConversationActor
		if err := rows.Scan(&a.UserID, &a.AgentKey); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan recipient: %w", err)
		}
		recipients = append(recipients, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list recipients: %w", err)
	}
	rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit send: %w", err)
	}
	return &models.ConversationSendResult{Message: *msg, Recipients: recipients}, nil
}

// conversationMessageSelect reads a message with its quote preview (TASK-4038).
// The 65536-character SQL bound covers every valid body under the 64 KiB byte
// cap, so QuoteExcerpt can skip any Unicode whitespace and collapse long runs
// within the first non-empty line. Quote reads remain bounded in both dialects.
const conversationMessageSelect = `
	SELECT m.id, m.conversation_id, m.seq, m.author_user_id, m.author_agent_key, m.source, m.body,
	       m.reply_to_seq, m.context_workspace_id, m.created_at, m.deleted_at,
	       q.seq, q.author_agent_key, substr(q.body, 1, 65536), q.deleted_at
	FROM conversation_messages m
	LEFT JOIN conversation_messages q ON q.conversation_id = m.conversation_id AND q.seq = m.reply_to_seq`

func scanConversationMessage(row rowScanner) (*models.ConversationMessage, error) {
	var m models.ConversationMessage
	var created string
	var deletedAt, ctxWS, qAgent, qBody, qDeleted sql.NullString
	var replyTo, qSeq sql.NullInt64
	if err := row.Scan(&m.ID, &m.ConversationID, &m.Seq, &m.AuthorUserID, &m.AuthorAgentKey, &m.Source, &m.Body,
		&replyTo, &ctxWS, &created, &deletedAt, &qSeq, &qAgent, &qBody, &qDeleted); err != nil {
		return nil, err
	}
	m.CreatedAt = parseTime(created)
	if replyTo.Valid {
		v := replyTo.Int64
		m.ReplyToSeq = &v
	}
	if ctxWS.Valid {
		v := ctxWS.String
		m.ContextWorkspaceID = &v
	}
	if deletedAt.Valid {
		m.Deleted = true
		m.DeletedAt = parseTimePtr(&deletedAt.String)
	}
	if qSeq.Valid {
		q := &models.ConversationQuote{Seq: qSeq.Int64, AuthorAgentKey: qAgent.String, AuthorIsHuman: qAgent.String == ""}
		if qDeleted.Valid {
			q.Deleted = true
		} else {
			q.Excerpt = models.QuoteExcerpt(qBody.String)
		}
		m.ReplyTo = q
	}
	return &m, nil
}

func (s *Store) conversationMessageBySeq(q Queryer, convID string, seq int64) (*models.ConversationMessage, error) {
	m, err := scanConversationMessage(q.QueryRow(s.q(conversationMessageSelect+` WHERE m.conversation_id = ? AND m.seq = ?`), convID, seq))
	if err != nil {
		return nil, fmt.Errorf("load message: %w", err)
	}
	return m, nil
}

// ListConversationMessages returns a window of messages in ascending seq
// order, tombstones included. With neither cursor it is the newest limit
// messages; before returns the newest limit below that seq; after returns the
// oldest limit above it. Both cursors are refused.
func (s *Store) ListConversationMessages(actor models.ConversationActor, convID string, before, after *int64, limit int) (*models.ConversationPage, error) {
	if err := validateConversationID(convID); err != nil {
		return nil, err
	}
	if err := validateConversationActor(actor); err != nil {
		return nil, err
	}
	if before != nil && after != nil {
		return nil, invalidConversation("before and after are mutually exclusive")
	}
	if limit <= 0 {
		limit = defaultConversationPage
	}
	if limit > maxConversationPage {
		limit = maxConversationPage
	}

	var lastSeq, readSeq int64
	err := s.db.QueryRow(s.q(`
		SELECT c.last_seq, member.read_seq FROM conversations c
		JOIN conversation_members member ON member.conversation_id = c.id
		WHERE c.id = ? AND member.user_id = ? AND member.agent_key = ?`),
		convID, actor.UserID, actor.AgentKey).Scan(&lastSeq, &readSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrConversationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("check membership: %w", err)
	}

	query := conversationMessageSelect + ` WHERE m.conversation_id = ?`
	args := []any{convID}
	order := "DESC"
	switch {
	case after != nil:
		query += ` AND m.seq > ?`
		args = append(args, *after)
		order = "ASC"
	case before != nil:
		query += ` AND m.seq < ?`
		args = append(args, *before)
	}
	query += ` ORDER BY m.seq ` + order + ` LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	var msgs []models.ConversationMessage
	for rows.Next() {
		m, err := scanConversationMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msgs = append(msgs, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	page := &models.ConversationPage{LastSeq: lastSeq, ReadSeq: readSeq}
	if len(msgs) > limit {
		page.HasMore = true
		msgs = msgs[:limit]
	}
	if order == "DESC" {
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}
	page.Messages = msgs
	if msgs == nil {
		page.Messages = []models.ConversationMessage{}
	}
	if len(msgs) > 0 {
		page.OldestSeq, page.NewestSeq = msgs[0].Seq, msgs[len(msgs)-1].Seq
	}
	return page, nil
}

// DeleteConversationMessage tombstones an authorized member's message while
// retaining its row and sequence, including on repeated deletes (TASK-4038).
func (s *Store) DeleteConversationMessage(actor models.ConversationActor, convID string, seq int64) (*models.ConversationMessage, bool, error) {
	if err := validateConversationID(convID); err != nil {
		return nil, false, err
	}
	if err := validateConversationActor(actor); err != nil {
		return nil, false, err
	}
	if seq < 1 {
		return nil, false, invalidConversation("seq must be at least 1")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin delete message: %w", err)
	}
	defer tx.Rollback()

	msg, err := scanConversationMessage(tx.QueryRow(s.q(conversationMessageSelect+`
		WHERE m.conversation_id = ? AND m.seq = ? AND EXISTS (
			SELECT 1 FROM conversation_members member WHERE member.conversation_id = m.conversation_id
			AND member.user_id = ? AND member.agent_key = ?)`), convID, seq, actor.UserID, actor.AgentKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrConversationNotFound
	}
	if err != nil {
		return nil, false, fmt.Errorf("load delete target: %w", err)
	}
	if msg.AuthorUserID != actor.UserID || (actor.AgentKey != "" && actor.AgentKey != msg.AuthorAgentKey) {
		return nil, false, ErrConversationForbidden
	}
	if msg.Deleted {
		return msg, false, nil
	}
	result, err := tx.Exec(s.q(`UPDATE conversation_messages
		SET body = '', deleted_at = ?, deleted_by_user_id = ?, deleted_by_agent_key = ?
		WHERE conversation_id = ? AND seq = ? AND deleted_at IS NULL`),
		chatNow(), actor.UserID, actor.AgentKey, convID, seq)
	if err != nil {
		return nil, false, fmt.Errorf("tombstone message: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("tombstone rows affected: %w", err)
	}
	msg, err = s.conversationMessageBySeq(tx, convID, seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrConversationNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit delete message: %w", err)
	}
	return msg, n != 0, nil
}

// MarkConversationRead advances the member's read marker, capped at last_seq (TASK-4038).
func (s *Store) MarkConversationRead(actor models.ConversationActor, convID string, toSeq int64) (int64, error) {
	if err := validateConversationID(convID); err != nil {
		return 0, err
	}
	if err := validateConversationActor(actor); err != nil {
		return 0, err
	}
	return s.markConversationSeq(actor, convID, toSeq, "read_seq")
}

// MarkConversationDelivered advances the delivery marker with the same cap (TASK-4038).
func (s *Store) MarkConversationDelivered(actor models.ConversationActor, convID string, toSeq int64) (int64, error) {
	if err := validateConversationID(convID); err != nil {
		return 0, err
	}
	if err := validateConversationActor(actor); err != nil {
		return 0, err
	}
	return s.markConversationSeq(actor, convID, toSeq, "delivered_seq")
}

// column is one of the two fixed marker columns above, never caller input (TASK-4038).
func (s *Store) markConversationSeq(actor models.ConversationActor, convID string, toSeq int64, column string) (int64, error) {
	if toSeq < 0 {
		return 0, invalidConversation("marker must not be negative")
	}
	var seq int64
	err := s.db.QueryRow(s.q(`UPDATE conversation_members SET `+column+` = CASE
		WHEN ? <= `+column+` THEN `+column+`
		WHEN ? > (SELECT last_seq FROM conversations WHERE id = ?) THEN (SELECT last_seq FROM conversations WHERE id = ?)
		ELSE ? END
		WHERE conversation_id = ? AND user_id = ? AND agent_key = ? RETURNING `+column),
		toSeq, toSeq, convID, convID, toSeq, convID, actor.UserID, actor.AgentKey).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrConversationNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("mark conversation %s: %w", column, err)
	}
	return seq, nil
}

// TASK-4038: a human and their agents share user_id, so authorship is a pair.
const conversationUnreadCount = `(SELECT COUNT(*) FROM conversation_messages x
	WHERE x.conversation_id = member.conversation_id AND x.seq > member.read_seq AND x.deleted_at IS NULL
	AND NOT (x.author_user_id = member.user_id AND x.author_agent_key = member.agent_key))`

// TASK-4038: each 2a DM has exactly one agent member, whose key labels the inbox row.
const conversationInboxFrom = ` FROM conversations c
	JOIN conversation_members member ON member.conversation_id = c.id
	JOIN conversation_members agent ON agent.conversation_id = c.id AND agent.agent_key <> ''
	WHERE member.user_id = ? AND member.agent_key = ?`

// UnreadSummary returns only memberships with unread messages and their total (TASK-4038).
func (s *Store) UnreadSummary(actor models.ConversationActor) ([]models.ConversationUnread, int, error) {
	if err := validateConversationActor(actor); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(s.q(`SELECT conversation_id, number, agent_key, unread, last_seq, read_seq, delivered_seq
		FROM (SELECT c.id AS conversation_id, c.number, agent.agent_key, `+conversationUnreadCount+` AS unread,
		c.last_seq, member.read_seq, member.delivered_seq`+conversationInboxFrom+`) inbox
		WHERE unread > 0 ORDER BY number`), actor.UserID, actor.AgentKey)
	if err != nil {
		return nil, 0, fmt.Errorf("unread summary: %w", err)
	}
	defer rows.Close()
	out := []models.ConversationUnread{}
	total := 0
	for rows.Next() {
		var u models.ConversationUnread
		if err := rows.Scan(&u.ConversationID, &u.Number, &u.AgentKey, &u.Unread, &u.LastSeq, &u.ReadSeq, &u.DeliveredSeq); err != nil {
			return nil, 0, fmt.Errorf("scan unread summary: %w", err)
		}
		out = append(out, u)
		total += u.Unread
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("unread summary rows: %w", err)
	}
	return out, total, nil
}

// ListConversations returns the member's inbox, with one batched read of the
// last messages; deleted previews remain tombstones (TASK-4038).
func (s *Store) ListConversations(actor models.ConversationActor) ([]models.ConversationSummary, error) {
	if err := validateConversationActor(actor); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(s.q(`SELECT c.id, c.owner_user_id, c.number, c.kind, c.title, c.dm_key,
		c.last_seq, c.created_at, c.updated_at, agent.agent_key, member.read_seq, member.delivered_seq, `+conversationUnreadCount+
		conversationInboxFrom+` ORDER BY c.updated_at DESC, c.id`), actor.UserID, actor.AgentKey)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	out := []models.ConversationSummary{}
	indexes := map[string]int{}
	var pairs []string
	var args []any
	for rows.Next() {
		var c models.ConversationSummary
		var dmKey sql.NullString
		var created, updated string
		if err := rows.Scan(&c.ID, &c.OwnerUserID, &c.Number, &c.Kind, &c.Title, &dmKey, &c.LastSeq,
			&created, &updated, &c.AgentKey, &c.ReadSeq, &c.DeliveredSeq, &c.Unread); err != nil {
			return nil, fmt.Errorf("scan conversation summary: %w", err)
		}
		c.DMKey = dmKey.String
		c.CreatedAt, c.UpdatedAt = parseTime(created), parseTime(updated)
		indexes[c.ID] = len(out)
		out = append(out, c)
		if c.LastSeq > 0 {
			pairs = append(pairs, "(?, ?)")
			args = append(args, c.ID, c.LastSeq)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list conversation rows: %w", err)
	}
	rows.Close()
	if len(pairs) == 0 {
		return out, nil
	}
	messages, err := s.db.Query(s.q(conversationMessageSelect+`
		WHERE (m.conversation_id, m.seq) IN (`+strings.Join(pairs, ", ")+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("list last messages: %w", err)
	}
	defer messages.Close()
	for messages.Next() {
		msg, err := scanConversationMessage(messages)
		if err != nil {
			return nil, fmt.Errorf("scan last message: %w", err)
		}
		out[indexes[msg.ConversationID]].LastMessage = msg
	}
	if err := messages.Err(); err != nil {
		return nil, fmt.Errorf("list last message rows: %w", err)
	}
	return out, nil
}

// ConversationMemberActors lists identities for a committed chat retraction
// (TASK-4041). Membership is checked just as on every other chat read.
func (s *Store) ConversationMemberActors(actor models.ConversationActor, convID string) ([]models.ConversationActor, error) {
	if _, err := s.GetConversation(actor, convID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(s.q(`SELECT user_id, agent_key FROM conversation_members WHERE conversation_id = ?`), convID)
	if err != nil {
		return nil, fmt.Errorf("list conversation members: %w", err)
	}
	defer rows.Close()
	var out []models.ConversationActor
	for rows.Next() {
		var a models.ConversationActor
		if err := rows.Scan(&a.UserID, &a.AgentKey); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
