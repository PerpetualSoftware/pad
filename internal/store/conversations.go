package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
)

const (
	maxConversationBodyBytes = 64 * 1024
	maxConversationSource    = 32
	maxConversationOpKey     = 128
	maxConversationAgentKey  = 128

	defaultConversationPage = 50
	maxConversationPage     = 200
)

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
	if len(key) > maxConversationAgentKey {
		return invalidConversation("agent key is longer than %d bytes", maxConversationAgentKey)
	}
	if !utf8.ValidString(key) {
		return invalidConversation("agent key is not valid UTF-8")
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return invalidConversation("agent key contains a control character")
		}
	}
	return nil
}

// EnsureAgentDM returns the owner's direct conversation with the agent,
// creating it (and its two member rows) on first use. created reports whether
// this call made it. Idempotent on dm_key.
func (s *Store) EnsureAgentDM(ownerUserID, agentKey string) (*models.Conversation, bool, error) {
	if ownerUserID == "" {
		return nil, false, invalidConversation("owner is required")
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

	id, ts := newID(), nowNano()
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
func (s *Store) SendConversationMessage(in models.ConversationSend) (*models.ConversationSendResult, error) {
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
	if in.Author.UserID == "" {
		return nil, invalidConversation("author is required")
	}
	if err := validateConversationAgentKey(in.Author.AgentKey, true); err != nil {
		return nil, err
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

func (s *Store) sendConversationMessageOnce(in models.ConversationSend) (*models.ConversationSendResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin send: %w", err)
	}
	defer tx.Rollback()

	ts := nowNano()
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
		err := tx.QueryRow(s.q(`
			SELECT seq FROM conversation_messages
			WHERE conversation_id = ? AND author_user_id = ? AND author_agent_key = ? AND op_key = ?
		`), in.ConversationID, in.Author.UserID, in.Author.AgentKey, in.OpKey).Scan(&prior)
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

	// The author has seen and received their own message.
	if _, err := tx.Exec(s.q(`
		UPDATE conversation_members SET
			read_seq = CASE WHEN read_seq < ? THEN ? ELSE read_seq END,
			delivered_seq = CASE WHEN delivered_seq < ? THEN ? ELSE delivered_seq END
		WHERE conversation_id = ? AND user_id = ? AND agent_key = ?
	`), seq, seq, seq, seq, in.ConversationID, in.Author.UserID, in.Author.AgentKey); err != nil {
		return nil, fmt.Errorf("advance author marker: %w", err)
	}

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

// conversationMessageSelect reads a message with its quote preview. The quote
// body is cut in SQL so a long quoted body is never carried whole.
const conversationMessageSelect = `
	SELECT m.id, m.conversation_id, m.seq, m.author_user_id, m.author_agent_key, m.source, m.body,
	       m.reply_to_seq, m.context_workspace_id, m.created_at, m.deleted_at,
	       q.seq, q.author_agent_key, substr(q.body, 1, 400), q.deleted_at
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
	if before != nil && after != nil {
		return nil, invalidConversation("before and after are mutually exclusive")
	}
	if limit <= 0 {
		limit = defaultConversationPage
	}
	if limit > maxConversationPage {
		limit = maxConversationPage
	}

	var lastSeq int64
	err := s.db.QueryRow(s.q(`
		SELECT c.last_seq FROM conversations c WHERE c.id = ? AND `+conversationMemberOf),
		convID, actor.UserID, actor.AgentKey).Scan(&lastSeq)
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

	page := &models.ConversationPage{LastSeq: lastSeq}
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
