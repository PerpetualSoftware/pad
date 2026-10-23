package store

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

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

func TestMembershipGatesEveryRead(t *testing.T) {
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
