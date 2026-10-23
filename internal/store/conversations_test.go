package store

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func chatUser(t *testing.T, s *Store, name string) *models.User {
	t.Helper()
	return createTestUser(t, s, name+"@chat.test", name, "password-"+name+"-12345")
}

func chatDM(t *testing.T, s *Store, owner *models.User, agent string) *models.Conversation {
	t.Helper()
	c, _, err := s.EnsureAgentDM(owner.ID, agent)
	if err != nil {
		t.Fatalf("EnsureAgentDM: %v", err)
	}
	return c
}

func chatSend(t *testing.T, s *Store, convID string, author models.ConversationActor, body, opKey string, replyTo *int64) *models.ConversationSendResult {
	t.Helper()
	r, err := s.SendConversationMessage(models.ConversationSend{ConversationID: convID, Author: author, Body: body, Source: "test", OpKey: opKey, ReplyToSeq: replyTo})
	if err != nil {
		t.Fatalf("send %q: %v", body, err)
	}
	return r
}

func chatCount(t *testing.T, s *Store, convID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM conversation_messages WHERE conversation_id = ?`), convID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func chatLastSeq(t *testing.T, s *Store, convID string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(s.q(`SELECT last_seq FROM conversations WHERE id = ?`), convID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestChatTimestampsAreFixedWidth(t *testing.T) {
	t.Run("format", func(t *testing.T) {
		instant := time.Date(2026, 10, 23, 5, 0, 5, 123456780, time.UTC)
		next := instant.Add(time.Nanosecond)
		first, second := chatTime(instant), chatTime(next)
		if len(first) != 30 || len(second) != 30 {
			t.Errorf("timestamp widths = %d/%d, want 30/30: %q, %q", len(first), len(second), first, second)
		}
		if !instant.Before(next) || first >= second {
			t.Errorf("timestamp order disagrees with time order: %q >= %q", first, second)
		}
		for _, value := range []time.Time{instant, next} {
			if got := parseTime(chatTime(value)); !got.Equal(value) {
				t.Errorf("parseTime round trip = %v, want %v", got, value)
			}
		}
	})
	t.Run("stored", func(t *testing.T) {
		s := testStore(t)
		u := chatUser(t, s, "alice")
		c := chatDM(t, s, u, "claude")
		human := models.ConversationActor{UserID: u.ID}
		first := chatSend(t, s, c.ID, human, "first", "", nil)
		chatSend(t, s, c.ID, human, "second", "", nil)
		if _, _, err := s.DeleteConversationMessage(human, c.ID, first.Message.Seq); err != nil {
			t.Fatal(err)
		}
		rows, err := s.db.Query(s.q(`
			SELECT 'conversations.created_at', created_at FROM conversations WHERE id = ?
			UNION ALL SELECT 'conversations.updated_at', updated_at FROM conversations WHERE id = ?
			UNION ALL SELECT 'conversation_members.added_at', added_at FROM conversation_members WHERE conversation_id = ?
			UNION ALL SELECT 'conversation_messages.created_at', created_at FROM conversation_messages WHERE conversation_id = ?
			UNION ALL SELECT 'conversation_messages.deleted_at', deleted_at FROM conversation_messages WHERE conversation_id = ? AND deleted_at IS NOT NULL
		`), c.ID, c.ID, c.ID, c.ID, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var column, value string
			if err := rows.Scan(&column, &value); err != nil {
				t.Fatal(err)
			}
			if len(value) != 30 {
				t.Errorf("%s width = %d, want 30: %q", column, len(value), value)
			}
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if count != 7 {
			t.Fatalf("timestamp count = %d, want 7", count)
		}
	})
}

func TestSendRejectsInvalidInput(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	actor := models.ConversationActor{UserID: u.ID}
	chatSend(t, s, c.ID, actor, "existing message", "", nil)

	for _, tc := range []struct {
		name                             string
		body, source, opKey, contextWSID string
	}{
		{name: "body NUL", body: "hello\x00world"},
		{name: "source NUL", body: "hello", source: "test\x00"},
		{name: "op_key NUL", body: "hello", opKey: "op\x00"},
		{name: "body invalid UTF-8", body: "hello\xff"},
		{name: "source invalid UTF-8", body: "hello", source: "test\xff"},
		{name: "op_key invalid UTF-8", body: "hello", opKey: "op\xff"},
		{name: "context NUL", body: "hello", contextWSID: "ws\x00"},
		{name: "context invalid UTF-8", body: "hello", contextWSID: "ws\xff"},
		{name: "context too long", body: "hello", contextWSID: strings.Repeat("x", 129)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := chatLastSeq(t, s, c.ID)
			_, err := s.SendConversationMessage(models.ConversationSend{
				ConversationID: c.ID, Author: actor, Body: tc.body, Source: tc.source,
				OpKey: tc.opKey, ContextWorkspaceID: tc.contextWSID,
			})
			if !errors.Is(err, ErrInvalidConversationInput) {
				t.Errorf("send: err = %v, want ErrInvalidConversationInput", err)
			}
			if got := chatLastSeq(t, s, c.ID); got != before {
				t.Errorf("last_seq = %d, want %d after refused send", got, before)
			}
		})
	}
}

func TestSendContextWorkspace(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	actor := models.ConversationActor{UserID: u.ID}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Context", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	in := models.ConversationSend{ConversationID: c.ID, Author: actor, Body: "context", ContextWorkspaceID: ws.ID}
	sent, err := s.SendConversationMessage(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := sent.Message.ContextWorkspaceID; got == nil || *got != ws.ID {
		t.Fatalf("stored context workspace = %v, want %s", got, ws.ID)
	}
	page, err := s.ListConversationMessages(actor, c.ID, nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := page.Messages[0].ContextWorkspaceID; got == nil || *got != ws.ID {
		t.Fatalf("listed context workspace = %v, want %s", got, ws.ID)
	}
	if err := s.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, id string }{
		{"unknown", "unknown-workspace"},
		{"soft-deleted", ws.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := chatLastSeq(t, s, c.ID)
			in.ContextWorkspaceID = tc.id
			if _, err := s.SendConversationMessage(in); !errors.Is(err, ErrInvalidConversationInput) {
				t.Errorf("send: err = %v, want ErrInvalidConversationInput", err)
			}
			if got := chatLastSeq(t, s, c.ID); got != before {
				t.Errorf("last_seq = %d, want %d after refused context", got, before)
			}
		})
	}
}

func TestDeleteRejectsNonPositiveSeq(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	actor := models.ConversationActor{UserID: u.ID}
	for _, seq := range []int64{0, -1} {
		t.Run(fmt.Sprint(seq), func(t *testing.T) {
			if _, _, err := s.DeleteConversationMessage(actor, c.ID, seq); !errors.Is(err, ErrInvalidConversationInput) {
				t.Errorf("delete seq %d: err = %v, want ErrInvalidConversationInput", seq, err)
			}
		})
	}
}

func TestQuoteExcerptLeadingWhitespace(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	actor := models.ConversationActor{UserID: u.ID}
	for _, tc := range []struct{ name, body, want string }{
		{"spaces and newlines", strings.Repeat(" \n", 500) + "hello", "hello"},
		{"body cap", strings.Repeat(" ", maxConversationBodyBytes-len("hello")) + "hello", "hello"},
		{"Unicode whitespace", strings.Repeat("\u2003\n", 250) + "hello", "hello"},
		{"whitespace within line", "hello" + strings.Repeat(" ", 1000) + "world", "hello world"},
		{"rune cap", strings.Repeat(" \n", 500) + strings.Repeat("é", 200), strings.Repeat("é", 97) + "..."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := chatSend(t, s, c.ID, actor, tc.body, "", nil)
			reply := chatSend(t, s, c.ID, actor, "reply", "", &original.Message.Seq)
			if q := reply.Message.ReplyTo; q == nil || q.Excerpt != tc.want {
				t.Errorf("quote = %+v, want excerpt %q", q, tc.want)
			}
			page, err := s.ListConversationMessages(actor, c.ID, nil, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if q := page.Messages[0].ReplyTo; q == nil || q.Excerpt != tc.want {
				t.Errorf("listed quote = %+v, want excerpt %q", q, tc.want)
			}
		})
	}
}

func TestConversationSeqConcurrent(t *testing.T) {
	run := func(t *testing.T, s *Store) {
		u := chatUser(t, s, "alice")
		c := chatDM(t, s, u, "claude")
		const n = 50
		var wg sync.WaitGroup
		seqs := make(chan int64, n)
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r, err := s.SendConversationMessage(models.ConversationSend{
					ConversationID: c.ID, Author: models.ConversationActor{UserID: u.ID},
					Body: fmt.Sprintf("m%d", i), Source: "test",
				})
				if err != nil {
					errs <- err
					return
				}
				seqs <- r.Message.Seq
			}(i)
		}
		wg.Wait()
		close(seqs)
		close(errs)
		for err := range errs {
			t.Fatalf("send: %v", err)
		}
		var got []int
		for q := range seqs {
			got = append(got, int(q))
		}
		sort.Ints(got)
		if len(got) != n {
			t.Fatalf("got %d seqs, want %d", len(got), n)
		}
		for i, q := range got {
			if q != i+1 {
				t.Fatalf("seqs not exactly 1..%d: %v", n, got)
			}
		}
		if ls := chatLastSeq(t, s, c.ID); ls != n {
			t.Fatalf("last_seq = %d, want %d", ls, n)
		}
		if cnt := chatCount(t, s, c.ID); cnt != n {
			t.Fatalf("rows = %d, want %d", cnt, n)
		}
	}
	t.Run("SQLite", func(t *testing.T) { run(t, testStoreSQLite(t)) })
	if pgURL := os.Getenv("PAD_TEST_POSTGRES_URL"); pgURL != "" {
		t.Run("PostgreSQL", func(t *testing.T) { run(t, testStorePostgres(t, pgURL)) })
	}
}

func TestSendIdempotentOpKey(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	human := models.ConversationActor{UserID: u.ID}
	agent := models.ConversationActor{UserID: u.ID, AgentKey: "claude"}

	first := chatSend(t, s, c.ID, human, "original", "op-1", nil)
	if first.Replayed {
		t.Fatal("first send reported as a replay")
	}
	// Ruling 9: a different body under the same key replays the stored message.
	again := chatSend(t, s, c.ID, human, "a different body", "op-1", nil)
	if !again.Replayed {
		t.Fatal("second send with the same op_key was not a replay")
	}
	if again.Message.ID != first.Message.ID || again.Message.Seq != first.Message.Seq {
		t.Fatalf("replay returned %s/%d, want %s/%d", again.Message.ID, again.Message.Seq, first.Message.ID, first.Message.Seq)
	}
	if again.Message.Body != "original" {
		t.Fatalf("replay body = %q, want the original", again.Message.Body)
	}
	if n := chatCount(t, s, c.ID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if ls := chatLastSeq(t, s, c.ID); ls != 1 {
		t.Fatalf("last_seq = %d after a replay, want 1 (no hole)", ls)
	}

	// The same key from another author is not a replay.
	other := chatSend(t, s, c.ID, agent, "agent message", "op-1", nil)
	if other.Replayed || other.Message.Seq != 2 {
		t.Fatalf("agent send with the human's key: replayed=%v seq=%d", other.Replayed, other.Message.Seq)
	}
	// Nor from another conversation.
	c2 := chatDM(t, s, u, "codex")
	if r := chatSend(t, s, c2.ID, human, "elsewhere", "op-1", nil); r.Replayed {
		t.Fatal("same key in another conversation was a replay")
	}

	// Concurrent sends with one key make one row.
	var wg sync.WaitGroup
	var mu sync.Mutex
	replays := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.SendConversationMessage(models.ConversationSend{ConversationID: c.ID, Author: human, Body: "race", OpKey: "op-race"})
			if err != nil {
				t.Errorf("send: %v", err)
				return
			}
			if r.Replayed {
				mu.Lock()
				replays++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if replays != 9 {
		t.Fatalf("replays = %d, want 9", replays)
	}
	if n := chatCount(t, s, c.ID); n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
	if ls := chatLastSeq(t, s, c.ID); ls != 3 {
		t.Fatalf("last_seq = %d, want 3", ls)
	}
}

func TestSendReplyTo(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	c2 := chatDM(t, s, u, "codex")
	human := models.ConversationActor{UserID: u.ID}
	agent := models.ConversationActor{UserID: u.ID, AgentKey: "claude"}

	long := "  first   line\twith   spaces " + strings.Repeat("é", 200) + "\nsecond line must not appear"
	chatSend(t, s, c.ID, agent, long, "", nil)
	one := int64(1)
	r := chatSend(t, s, c.ID, human, "reply", "", &one)
	q := r.Message.ReplyTo
	if q == nil || q.Seq != 1 || q.AuthorAgentKey != "claude" || q.AuthorIsHuman || q.Deleted {
		t.Fatalf("bad quote: %+v", q)
	}
	if n := len([]rune(q.Excerpt)); n == 0 || n > models.ConversationExcerptRunes {
		t.Fatalf("excerpt is %d runes: %q", n, q.Excerpt)
	}
	if strings.Contains(q.Excerpt, "second line") || !strings.HasPrefix(q.Excerpt, "first line with spaces ") {
		t.Fatalf("excerpt = %q", q.Excerpt)
	}
	// The listing computes the same quote.
	page, err := s.ListConversationMessages(human, c.ID, nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if lq := page.Messages[1].ReplyTo; lq == nil || lq.Excerpt != q.Excerpt {
		t.Fatalf("listed quote = %+v", lq)
	}

	// A seq that exists only in another conversation is not a target here.
	for i := 0; i < 3; i++ {
		chatSend(t, s, c2.ID, human, fmt.Sprintf("other %d", i), "", nil)
	}
	three := int64(3)
	before := chatLastSeq(t, s, c.ID)
	if _, err = s.SendConversationMessage(models.ConversationSend{ConversationID: c.ID, Author: human, Body: "x", ReplyToSeq: &three}); !errors.Is(err, ErrReplyTargetNotFound) {
		t.Fatalf("cross-conversation target: err = %v", err)
	}
	if ls := chatLastSeq(t, s, c.ID); ls != before {
		t.Fatalf("a refused send spent a seq: last_seq = %d, want %d", ls, before)
	}

	// TASK-4038: reject self and future replies before the schema CHECK can fire.
	for _, offset := range []int64{1, 50} {
		t.Run(fmt.Sprintf("last_seq+%d", offset), func(t *testing.T) {
			before := chatLastSeq(t, s, c.ID)
			target := before + offset
			if _, err := s.SendConversationMessage(models.ConversationSend{
				ConversationID: c.ID, Author: human, Body: "future reply", ReplyToSeq: &target,
			}); !errors.Is(err, ErrReplyTargetNotFound) {
				t.Errorf("reply to %d: err = %v, want ErrReplyTargetNotFound", target, err)
			}
			if got := chatLastSeq(t, s, c.ID); got != before {
				t.Errorf("last_seq = %d, want %d after refused reply", got, before)
			}
		})
	}

	// A tombstoned target (set by raw UPDATE: delete is run B).
	if _, err := s.db.Exec(s.q(`UPDATE conversation_messages SET body = '', deleted_at = ? WHERE conversation_id = ? AND seq = 1`), nowNano(), c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SendConversationMessage(models.ConversationSend{ConversationID: c.ID, Author: human, Body: "x", ReplyToSeq: &one}); !errors.Is(err, ErrReplyTargetDeleted) {
		t.Fatalf("tombstoned target: err = %v", err)
	}
	// The earlier reply now quotes a deleted message with no excerpt.
	page, _ = s.ListConversationMessages(human, c.ID, nil, nil, 10)
	if dq := page.Messages[1].ReplyTo; dq == nil || !dq.Deleted || dq.Excerpt != "" {
		t.Fatalf("quote of a tombstone = %+v", dq)
	}

	// reply_to_seq >= seq is refused by the schema.
	if _, err := s.db.Exec(s.q(`INSERT INTO conversation_messages (id, conversation_id, seq, author_user_id, body, reply_to_seq, created_at) VALUES (?, ?, 99, ?, 'b', 99, ?)`),
		newID(), c.ID, u.ID, nowNano()); err == nil {
		t.Fatal("reply_to_seq = seq was accepted")
	}
}

func TestSendAdvancesOwnMarker(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	r := chatSend(t, s, c.ID, models.ConversationActor{UserID: u.ID}, "hi", "", nil)
	marks := func(agent string) (int64, int64) {
		var rs, ds int64
		if err := s.db.QueryRow(s.q(`SELECT read_seq, delivered_seq FROM conversation_members WHERE conversation_id = ? AND user_id = ? AND agent_key = ?`), c.ID, u.ID, agent).Scan(&rs, &ds); err != nil {
			t.Fatal(err)
		}
		return rs, ds
	}
	if rs, ds := marks(""); rs != r.Message.Seq || ds != r.Message.Seq {
		t.Fatalf("author markers = %d/%d, want %d/%d", rs, ds, r.Message.Seq, r.Message.Seq)
	}
	if rs, ds := marks("claude"); rs != 0 || ds != 0 {
		t.Fatalf("recipient markers moved: %d/%d", rs, ds)
	}
	if len(r.Recipients) != 1 || r.Recipients[0].AgentKey != "claude" {
		t.Fatalf("recipients = %+v", r.Recipients)
	}
}

func TestDmKeyUnique(t *testing.T) {
	s := testStore(t)
	a, b := chatUser(t, s, "alice"), chatUser(t, s, "bob")

	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	ids := map[string]bool{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, made, err := s.EnsureAgentDM(a.ID, "claude")
			if err != nil {
				t.Errorf("EnsureAgentDM: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[c.ID] = true
			if made {
				created++
			}
		}()
	}
	wg.Wait()
	if created != 1 || len(ids) != 1 {
		t.Fatalf("created=%d distinct=%d, want 1/1", created, len(ids))
	}
	c1 := chatDM(t, s, a, "claude")
	c2 := chatDM(t, s, a, "codex")
	if c1.Number != 1 || c2.Number != 2 {
		t.Fatalf("numbers = %d, %d; want 1, 2", c1.Number, c2.Number)
	}
	if cb := chatDM(t, s, b, "claude"); cb.Number != 1 || cb.ID == c1.ID {
		t.Fatalf("second owner's DM = %+v", cb)
	}
	if _, err := s.db.Exec(s.q(`INSERT INTO conversations (id, owner_user_id, number, dm_key, created_at, updated_at) VALUES (?, ?, 50, ?, ?, ?)`),
		newID(), a.ID, c1.DMKey, nowNano(), nowNano()); err == nil || !isUniqueViolation(err) {
		t.Fatalf("duplicate dm_key: err = %v", err)
	}
	for _, bad := range []string{"", strings.Repeat("a", 129), "a\nb"} {
		if _, _, err := s.EnsureAgentDM(a.ID, bad); !errors.Is(err, ErrInvalidConversationInput) {
			t.Fatalf("agent key %q: err = %v", bad, err)
		}
	}
}

// TASK-4038: EnsureAgentDM is excluded because it takes no actor; its caller (TASK-4039) must pass the authenticated owner.
func TestMembershipGatesEveryActorMethod(t *testing.T) {
	s := testStore(t)
	a, b := chatUser(t, s, "alice"), chatUser(t, s, "bob")
	c := chatDM(t, s, a, "claude")
	chatSend(t, s, c.ID, models.ConversationActor{UserID: a.ID}, "hi", "", nil)

	outsiders := []models.ConversationActor{
		{UserID: b.ID},
		{UserID: b.ID, AgentKey: "claude"},
		{UserID: a.ID, AgentKey: "other-agent"},
	}
	for _, o := range outsiders {
		if _, err := s.GetConversation(o, c.ID); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("%+v GetConversation: %v", o, err)
		}
		if _, err := s.GetConversationByNumber(o, c.Number); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("%+v GetConversationByNumber: %v", o, err)
		}
		if _, err := s.ListConversationMessages(o, c.ID, nil, nil, 10); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("%+v ListConversationMessages: %v", o, err)
		}
		if _, err := s.SendConversationMessage(models.ConversationSend{ConversationID: c.ID, Author: o, Body: "x"}); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("%+v Send: %v", o, err)
		}
		if _, _, err := s.DeleteConversationMessage(o, c.ID, 1); !errors.Is(err, ErrConversationNotFound) {
			t.Errorf("%+v Delete: %v", o, err)
		}
		for _, mark := range []func(models.ConversationActor, string, int64) (int64, error){s.MarkConversationRead, s.MarkConversationDelivered} {
			if _, err := mark(o, c.ID, 1); !errors.Is(err, ErrConversationNotFound) {
				t.Errorf("%+v Mark: %v", o, err)
			}
		}
		if rows, err := s.ListConversations(o); err != nil || len(rows) != 0 {
			t.Errorf("%+v List: %+v, %v", o, rows, err)
		}
		if rows, total, err := s.UnreadSummary(o); err != nil || len(rows) != 0 || total != 0 {
			t.Errorf("%+v Unread: %+v total=%d, %v", o, rows, total, err)
		}
	}
	if n := chatCount(t, s, c.ID); n != 1 {
		t.Fatalf("an outsider's send left a row: %d", n)
	}
	if ls := chatLastSeq(t, s, c.ID); ls != 1 {
		t.Fatalf("an outsider's send spent a seq: %d", ls)
	}
	// Members read it, by id and by number.
	for _, m := range []models.ConversationActor{{UserID: a.ID}, {UserID: a.ID, AgentKey: "claude"}} {
		if _, err := s.GetConversation(m, c.ID); err != nil {
			t.Errorf("%+v GetConversation: %v", m, err)
		}
		if got, err := s.GetConversationByNumber(m, c.Number); err != nil || got.ID != c.ID {
			t.Errorf("%+v GetConversationByNumber: %v", m, err)
		}
	}
}

func TestListMessagesPaging(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	me := models.ConversationActor{UserID: u.ID}
	for i := 1; i <= 10; i++ {
		chatSend(t, s, c.ID, me, fmt.Sprintf("m%d", i), "", nil)
	}
	// A tombstone stays in the page.
	if _, err := s.db.Exec(s.q(`UPDATE conversation_messages SET body = '', deleted_at = ? WHERE conversation_id = ? AND seq = 8`), nowNano(), c.ID); err != nil {
		t.Fatal(err)
	}
	seqs := func(p *models.ConversationPage) []int64 {
		var out []int64
		for _, m := range p.Messages {
			out = append(out, m.Seq)
		}
		return out
	}
	eq := func(got, want []int64) bool { return fmt.Sprint(got) == fmt.Sprint(want) }
	i64 := func(v int64) *int64 { return &v }

	p, err := s.ListConversationMessages(me, c.ID, nil, nil, 4)
	if err != nil || !eq(seqs(p), []int64{7, 8, 9, 10}) || !p.HasMore || p.OldestSeq != 7 || p.NewestSeq != 10 || p.LastSeq != 10 {
		t.Fatalf("newest: %v %+v", err, p)
	}
	if p.ReadSeq != 10 {
		t.Fatalf("page read_seq = %d, want 10", p.ReadSeq)
	}
	if !p.Messages[1].Deleted || p.Messages[1].Body != "" {
		t.Fatalf("tombstone not returned as such: %+v", p.Messages[1])
	}
	p, err = s.ListConversationMessages(me, c.ID, i64(7), nil, 4)
	if err != nil || !eq(seqs(p), []int64{3, 4, 5, 6}) || !p.HasMore {
		t.Fatalf("before 7: %v %v", err, seqs(p))
	}
	p, err = s.ListConversationMessages(me, c.ID, i64(3), nil, 4)
	if err != nil || !eq(seqs(p), []int64{1, 2}) || p.HasMore {
		t.Fatalf("before 3: %v %v", err, seqs(p))
	}
	p, err = s.ListConversationMessages(me, c.ID, nil, i64(2), 3)
	if err != nil || !eq(seqs(p), []int64{3, 4, 5}) || !p.HasMore {
		t.Fatalf("after 2: %v %v", err, seqs(p))
	}
	p, err = s.ListConversationMessages(me, c.ID, nil, i64(10), 3)
	if err != nil || len(p.Messages) != 0 || p.HasMore || p.Messages == nil {
		t.Fatalf("after 10: %v %+v", err, p)
	}
	if _, err = s.ListConversationMessages(me, c.ID, i64(5), i64(2), 3); !errors.Is(err, ErrInvalidConversationInput) {
		t.Fatalf("both cursors: %v", err)
	}
	p, err = s.ListConversationMessages(me, c.ID, nil, nil, 0)
	if err != nil || len(p.Messages) != 10 || p.HasMore {
		t.Fatalf("default limit: %v %d", err, len(p.Messages))
	}
}

func TestDeleteAlwaysTombstones(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	human := models.ConversationActor{UserID: u.ID}
	agent := models.ConversationActor{UserID: u.ID, AgentKey: "claude"}
	original := chatSend(t, s, c.ID, agent, "erase me", "", nil).Message
	chatSend(t, s, c.ID, human, "reply", "", &original.Seq)
	deleted, changed, err := s.DeleteConversationMessage(agent, c.ID, original.Seq)
	if err != nil || !changed || deleted == nil || deleted.ID != original.ID || deleted.Seq != original.Seq || !deleted.Deleted || deleted.Body != "" || deleted.DeletedAt == nil {
		t.Fatalf("delete: message=%+v changed=%v err=%v", deleted, changed, err)
	}
	var body, deletedAt, deletedBy, deletedAgent string
	if err := s.db.QueryRow(s.q(`SELECT body, deleted_at, deleted_by_user_id, deleted_by_agent_key FROM conversation_messages WHERE id = ?`), original.ID).Scan(&body, &deletedAt, &deletedBy, &deletedAgent); err != nil {
		t.Fatal(err)
	}
	if body != "" || deletedAt == "" || deletedBy != u.ID || deletedAgent != agent.AgentKey {
		t.Fatalf("tombstone fields: %q %q %q %q", body, deletedAt, deletedBy, deletedAgent)
	}
	again, changed, err := s.DeleteConversationMessage(human, c.ID, original.Seq)
	if err != nil || changed || again == nil || !again.Deleted || !again.DeletedAt.Equal(*deleted.DeletedAt) {
		t.Fatalf("second delete: %+v changed=%v err=%v", again, changed, err)
	}
	var afterBy, afterAgent string
	if err := s.db.QueryRow(s.q(`SELECT deleted_by_user_id, deleted_by_agent_key FROM conversation_messages WHERE id = ?`), original.ID).Scan(&afterBy, &afterAgent); err != nil {
		t.Fatal(err)
	}
	if afterBy != deletedBy || afterAgent != deletedAgent || chatCount(t, s, c.ID) != 2 || chatLastSeq(t, s, c.ID) != 2 {
		t.Fatal("second delete changed attribution, row count or last_seq")
	}
	page, err := s.ListConversationMessages(human, c.ID, nil, nil, 10)
	if err != nil || len(page.Messages) != 2 || !page.Messages[0].Deleted || page.Messages[0].Body != "" {
		t.Fatalf("listed tombstone: %+v err=%v", page, err)
	}
	if q := page.Messages[1].ReplyTo; q == nil || !q.Deleted || q.Excerpt != "" {
		t.Fatalf("quote of tombstone: %+v", q)
	}
}

func TestSendReplayAfterTombstone(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	actor := models.ConversationActor{UserID: u.ID}
	first := chatSend(t, s, c.ID, actor, "original", "op-1", nil)
	if _, _, err := s.DeleteConversationMessage(actor, c.ID, first.Message.Seq); err != nil {
		t.Fatal(err)
	}
	replay := chatSend(t, s, c.ID, actor, "different", "op-1", nil)
	if !replay.Replayed || replay.Message.ID != first.Message.ID || replay.Message.Seq != first.Message.Seq || !replay.Message.Deleted || replay.Message.Body != "" || replay.Message.DeletedAt == nil || chatCount(t, s, c.ID) != 1 || chatLastSeq(t, s, c.ID) != 1 {
		t.Fatalf("replay after delete: %+v", replay)
	}
}

func TestDeleteAuthorization(t *testing.T) {
	s := testStore(t)
	u, outsider := chatUser(t, s, "alice"), chatUser(t, s, "bob")
	c := chatDM(t, s, u, "claude")
	chatDM(t, s, u, "codex")
	human := models.ConversationActor{UserID: u.ID}
	agent := models.ConversationActor{UserID: u.ID, AgentKey: "claude"}
	cases := []struct {
		name          string
		author, actor models.ConversationActor
		want          error
	}{
		{"owner deletes agent", agent, human, nil},
		{"agent deletes own", agent, agent, nil},
		{"owner deletes own", human, human, nil},
		{"agent cannot delete human", human, agent, ErrConversationForbidden},
		{"other agent is not a member", agent, models.ConversationActor{UserID: u.ID, AgentKey: "codex"}, ErrConversationNotFound},
		{"other user is not a member", agent, models.ConversationActor{UserID: outsider.ID}, ErrConversationNotFound},
	}
	// TASK-4038: these are all member/author combinations in a two-member DM.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := chatSend(t, s, c.ID, tc.author, tc.name, "", nil).Message
			got, changed, err := s.DeleteConversationMessage(tc.actor, c.ID, msg.Seq)
			if !errors.Is(err, tc.want) || (tc.want == nil && (!changed || got == nil || !got.Deleted)) || (tc.want != nil && (changed || got != nil)) {
				t.Fatalf("delete: %+v changed=%v err=%v, want %v", got, changed, err, tc.want)
			}
			if tc.want != nil {
				stored, err := s.conversationMessageBySeq(s.db, c.ID, msg.Seq)
				if err != nil || stored.Deleted || stored.Body != tc.name {
					t.Fatalf("refused delete changed message: %+v err=%v", stored, err)
				}
			}
		})
	}
	if _, _, err := s.DeleteConversationMessage(human, c.ID, 999); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("missing seq: %v", err)
	}
}

func TestReadSeqMonotonic(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	human := models.ConversationActor{UserID: u.ID}
	agent := models.ConversationActor{UserID: u.ID, AgentKey: "claude"}
	for _, tc := range []struct {
		name string
		mark func(models.ConversationActor, string, int64) (int64, error)
	}{{"read", s.MarkConversationRead}, {"delivered", s.MarkConversationDelivered}} {
		t.Run(tc.name, func(t *testing.T) {
			c := chatDM(t, s, u, "claude-"+tc.name)
			for i := 0; i < 7; i++ {
				chatSend(t, s, c.ID, models.ConversationActor{UserID: u.ID, AgentKey: "claude-" + tc.name}, "hi", "", nil)
			}
			for _, step := range []struct{ to, want int64 }{{5, 5}, {3, 5}, {999, 7}, {0, 7}} {
				if got, err := tc.mark(human, c.ID, step.to); err != nil || got != step.want {
					t.Fatalf("mark %d = %d, want %d: %v", step.to, got, step.want, err)
				}
				page, err := s.ListConversationMessages(human, c.ID, nil, nil, 3)
				if err != nil {
					t.Fatal(err)
				}
				wantRead := int64(0)
				if tc.name == "read" {
					wantRead = step.want
				}
				if page.ReadSeq != wantRead {
					t.Fatalf("page read_seq after %s %d = %d, want %d", tc.name, step.to, page.ReadSeq, wantRead)
				}
			}
			empty := chatDM(t, s, u, "empty-"+tc.name)
			if got, err := tc.mark(human, empty.ID, 999); err != nil || got != 0 {
				t.Fatalf("empty marker = %d: %v", got, err)
			}
			if _, err := tc.mark(human, c.ID, -1); !errors.Is(err, ErrInvalidConversationInput) {
				t.Fatalf("negative marker: %v", err)
			}
			if _, err := tc.mark(agent, c.ID, 1); !errors.Is(err, ErrConversationNotFound) {
				t.Fatalf("non-member marker: %v", err)
			}
			if _, err := tc.mark(human, "missing", 1); !errors.Is(err, ErrConversationNotFound) {
				t.Fatalf("missing conversation marker: %v", err)
			}
		})
	}
}

func TestUnreadExcludesOwnAndDeleted(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c, c2 := chatDM(t, s, u, "claude"), chatDM(t, s, u, "codex")
	human := models.ConversationActor{UserID: u.ID}
	agent := models.ConversationActor{UserID: u.ID, AgentKey: "claude"}
	chatSend(t, s, c.ID, human, "human message", "", nil)
	chatSend(t, s, c.ID, agent, "agent message", "", nil)
	deleted := chatSend(t, s, c.ID, agent, "deleted message", "", nil).Message
	if _, _, err := s.DeleteConversationMessage(human, c.ID, deleted.Seq); err != nil {
		t.Fatal(err)
	}
	chatSend(t, s, c2.ID, models.ConversationActor{UserID: u.ID, AgentKey: "codex"}, "other DM", "", nil)
	// TASK-4038: isolate the unread predicate from the sender's automatic marker.
	if _, err := s.db.Exec(s.q(`UPDATE conversation_members SET read_seq = 0 WHERE conversation_id = ?`), c.ID); err != nil {
		t.Fatal(err)
	}
	check := func(actor models.ConversationActor, wantRows, wantTotal int) []models.ConversationUnread {
		t.Helper()
		rows, total, err := s.UnreadSummary(actor)
		if err != nil || len(rows) != wantRows || total != wantTotal {
			t.Fatalf("unread %q: rows=%+v total=%d, want %d/%d: %v", actor.AgentKey, rows, total, wantRows, wantTotal, err)
		}
		return rows
	}
	rows := check(human, 2, 2)
	for _, row := range rows {
		if row.Unread != 1 || row.ReadSeq != 0 || (row.ConversationID == c.ID && (row.Number != c.Number || row.AgentKey != "claude" || row.LastSeq != 3 || row.DeliveredSeq != 1)) {
			t.Fatalf("human unread row: %+v", row)
		}
	}
	rows = check(agent, 1, 1)
	if rows[0].ConversationID != c.ID || rows[0].AgentKey != "claude" || rows[0].DeliveredSeq != 3 {
		t.Fatalf("agent unread row: %+v", rows[0])
	}
	for _, actor := range []models.ConversationActor{human, agent} {
		if _, err := s.MarkConversationRead(actor, c.ID, 999); err != nil {
			t.Fatal(err)
		}
	}
	check(agent, 0, 0)
	check(human, 1, 1)
	if _, err := s.MarkConversationRead(human, c2.ID, 999); err != nil {
		t.Fatal(err)
	}
	check(human, 0, 0)
	check(models.ConversationActor{UserID: u.ID, AgentKey: "outsider"}, 0, 0)
}

func TestListConversations(t *testing.T) {
	s := testStore(t)
	u, other := chatUser(t, s, "alice"), chatUser(t, s, "bob")
	human := models.ConversationActor{UserID: u.ID}
	c1, c2 := chatDM(t, s, u, "claude"), chatDM(t, s, u, "codex")
	chatSend(t, s, c2.ID, models.ConversationActor{UserID: u.ID, AgentKey: "codex"}, "older", "", nil)
	first := chatSend(t, s, c1.ID, human, "human", "", nil).Message
	last := chatSend(t, s, c1.ID, models.ConversationActor{UserID: u.ID, AgentKey: "claude"}, "newest", "", &first.Seq).Message
	// TASK-4038: the highest seq remains the preview even with the oldest timestamp.
	if _, err := s.db.Exec(s.q(`UPDATE conversation_messages SET created_at = ? WHERE id = ?`), "2000-01-01T00:00:00.000000000Z", last.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListConversations(human)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list: %+v err=%v", rows, err)
	}
	if rows[0].ID != c1.ID || rows[1].ID != c2.ID || rows[0].AgentKey != "claude" || rows[1].AgentKey != "codex" || rows[0].Unread != 1 || rows[1].Unread != 1 || rows[0].ReadSeq != 1 || rows[1].ReadSeq != 0 {
		t.Fatalf("inbox order/counts: %+v", rows)
	}
	if m := rows[0].LastMessage; m == nil || m.ID != last.ID || m.Seq != 2 || m.Body != "newest" || m.ReplyTo == nil || m.ReplyTo.Excerpt != "human" {
		t.Fatalf("last message: %+v", m)
	}
	if !rows[0].LastMessage.CreatedAt.Before(first.CreatedAt) {
		t.Fatalf("last message timestamp = %v, want before %v", rows[0].LastMessage.CreatedAt, first.CreatedAt)
	}
	if m := rows[1].LastMessage; m == nil || m.Seq != 1 || m.Body != "older" {
		t.Fatalf("other last message: %+v", m)
	}
	if _, _, err := s.DeleteConversationMessage(human, c1.ID, last.Seq); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListConversations(human)
	if err != nil || len(rows) != 2 || rows[0].ID != c1.ID || rows[0].Unread != 0 || rows[0].LastMessage == nil || !rows[0].LastMessage.Deleted || rows[0].LastMessage.Body != "" {
		t.Fatalf("tombstoned last message: %+v err=%v", rows, err)
	}
	agentRows, err := s.ListConversations(models.ConversationActor{UserID: u.ID, AgentKey: "claude"})
	if err != nil || len(agentRows) != 1 || agentRows[0].ID != c1.ID || agentRows[0].ReadSeq != 2 || agentRows[0].Unread != 0 {
		t.Fatalf("agent inbox: %+v err=%v", agentRows, err)
	}
	if rows, err := s.ListConversations(models.ConversationActor{UserID: other.ID}); err != nil || len(rows) != 0 {
		t.Fatalf("other owner: %+v err=%v", rows, err)
	}
	// TASK-4038: equal timestamps have a stable id tie-breaker, empty DMs no preview.
	empty := chatDM(t, s, u, "empty")
	if _, err := s.db.Exec(s.q(`UPDATE conversations SET updated_at = ? WHERE owner_user_id = ?`), nowNano(), u.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListConversations(human)
	if err != nil || len(rows) != 3 || rows[0].ID > rows[1].ID || rows[1].ID > rows[2].ID {
		t.Fatalf("tied order: %+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.ID == empty.ID && (row.LastMessage != nil || row.Unread != 0 || row.LastSeq != 0) {
			t.Fatalf("empty DM: %+v", row)
		}
	}
}

func TestDeleteAccountRemovesConversations(t *testing.T) {
	s := testStore(t)
	u, bystander := chatUser(t, s, "alice"), chatUser(t, s, "bob")
	c, kept := chatDM(t, s, u, "claude"), chatDM(t, s, bystander, "claude")
	chatSend(t, s, c.ID, models.ConversationActor{UserID: u.ID}, "doomed", "", nil)
	chatSend(t, s, kept.ID, models.ConversationActor{UserID: bystander.ID}, "kept", "", nil)
	if err := s.DeleteAccountAtomic(u.ID); err != nil {
		t.Fatal(err)
	}
	assertChatRows(t, s, c.ID, 0, 0, 0)
	assertChatRows(t, s, kept.ID, 1, 2, 1)
	if got, err := s.GetUser(u.ID); err != nil || got != nil {
		t.Fatalf("deleted user: %+v err=%v", got, err)
	}
}

func TestAccountClaimRemovesConversations(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "claim@chat.test")
	bystander := chatUser(t, s, "bob")
	c, kept := chatDM(t, s, u, "claude"), chatDM(t, s, bystander, "claude")
	chatSend(t, s, c.ID, models.ConversationActor{UserID: u.ID}, "old owner's chat", "", nil)
	chatSend(t, s, kept.ID, models.ConversationActor{UserID: bystander.ID}, "kept", "", nil)
	token, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimAccountByVerification(token)
	if err != nil || claim.UserID != u.ID {
		t.Fatalf("claim: %+v err=%v", claim, err)
	}
	assertChatRows(t, s, c.ID, 0, 0, 0)
	assertChatRows(t, s, kept.ID, 1, 2, 1)
	if got, err := s.GetUser(u.ID); err != nil || got == nil || !got.IsEmailVerified() {
		t.Fatalf("claimed user row: %+v err=%v", got, err)
	}
}

func assertChatRows(t *testing.T, s *Store, convID string, conversations, members, messages int) {
	t.Helper()
	for _, tc := range []struct {
		query string
		want  int
	}{
		{`SELECT COUNT(*) FROM conversations WHERE id = ?`, conversations},
		{`SELECT COUNT(*) FROM conversation_members WHERE conversation_id = ?`, members},
		{`SELECT COUNT(*) FROM conversation_messages WHERE conversation_id = ?`, messages},
	} {
		var got int
		if err := s.db.QueryRow(s.q(tc.query), convID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.query, got, tc.want)
		}
	}
}

func TestPurgeWorkspaceLeavesChatMessages(t *testing.T) {
	s := testStore(t)
	u := chatUser(t, s, "alice")
	c := chatDM(t, s, u, "claude")
	actor := models.ConversationActor{UserID: u.ID}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Context", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	in := models.ConversationSend{ConversationID: c.ID, Author: actor, Body: "workspace context", ContextWorkspaceID: ws.ID}
	sent, err := s.SendConversationMessage(in)
	if err != nil || sent.Message.ContextWorkspaceID == nil || *sent.Message.ContextWorkspaceID != ws.ID {
		t.Fatalf("send with context: %+v err=%v", sent, err)
	}
	if err := s.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeWorkspaceData(ws.ID); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListConversationMessages(actor, c.ID, nil, nil, 10)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != sent.Message.ID || page.Messages[0].ContextWorkspaceID != nil || page.Messages[0].Body != in.Body || page.Messages[0].Deleted {
		t.Fatalf("message after purge: %+v err=%v", page, err)
	}
	in.ContextWorkspaceID = "unknown-workspace"
	if _, err := s.SendConversationMessage(in); err == nil {
		t.Fatal("send with unknown workspace accepted")
	}
	if chatCount(t, s, c.ID) != 1 || chatLastSeq(t, s, c.ID) != 1 {
		t.Fatal("failed FK send changed messages or last_seq")
	}
}
