package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4039: drive the real router, including authentication, CSRF and limits.
type chatFixture struct{ *accessFixture }

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	return &chatFixture{newAccessFixture(t, store.DriverSQLite)}
}

const chatBase = "/api/v1/me/conversations"

func (f *chatFixture) request(method, path, agent string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.ownerTok)
	r.Header.Set("X-Pad-Agent", agent)
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, r)
	return rr
}
func (f *chatFixture) ensure(agent string) string {
	f.t.Helper()
	rr := f.request("POST", chatBase, "", map[string]any{"agent_key": agent})
	f.must(rr, 201, "ensure")
	var out struct{ Conversation struct{ Number int } }
	parseJSON(f.t, rr, &out)
	return fmt.Sprintf("%s/%d", chatBase, out.Conversation.Number)
}
func chatObject(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	parseJSON(t, rr, &out)
	return out
}
func chatMessage(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return chatObject(t, rr)["message"].(map[string]any)
}
func TestChat_EnsureDM_CreateThenIdempotent(t *testing.T) {
	f := newChatFixture(t)
	first := f.request("POST", chatBase, "", map[string]any{"agent_key": "rook"})
	f.must(first, 201, "create")
	next := f.request("POST", chatBase, "rook", map[string]any{})
	f.must(next, 200, "existing")
	a, b := chatObject(t, first), chatObject(t, next)
	if a["created"] != true || b["created"] != false || a["conversation"].(map[string]any)["id"] != b["conversation"].(map[string]any)["id"] {
		t.Fatalf("create/reuse: %v %v", a, b)
	}
	for _, body := range []any{map[string]any{"agent_key": "other"}, map[string]any{"agent_key": strings.Repeat("x", 129)}} {
		f.must(f.request("POST", chatBase, "rook", body), 400, "mismatch")
	}
	f.must(f.request("POST", chatBase, "", map[string]any{}), 400, "missing key")
}

// TASK-4039 r3: human-created DMs must be reachable through X-Pad-Agent.
func TestChat_Ensure_RejectsUnnormalisedAgentKey(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "")
	f := newChatFixture(t)
	token, err := f.srv.store.CreateSession(f.owner.ID, "web", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"leading whitespace", " claude"},
		{"trailing whitespace", "claude "},
		{"over rune cap", strings.Repeat("x", models.MaxAgentNameRunes+1)},
		{"embedded control", "cla\u0007ude"},
		{"normalised", "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"agent_key": tc.key})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", chatBase, strings.NewReader(string(body)))
			r.AddCookie(&http.Cookie{Name: "pad_session", Value: token})
			r.AddCookie(&http.Cookie{Name: "pad_csrf", Value: strings.Repeat("a", 64)})
			r.Header.Set("X-CSRF-Token", strings.Repeat("a", 64))
			r.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			f.srv.ServeHTTP(rr, r)
			if tc.key == "claude" {
				if rr.Code != 201 {
					t.Fatalf("valid key: %d %s", rr.Code, rr.Body.String())
				}
				return
			}
			if rr.Code != 400 {
				t.Errorf("invalid key: got %d, want 400: %s", rr.Code, rr.Body.String())
			} else {
				errorBody := chatObject(t, rr)["error"].(map[string]any)
				if errorBody["code"] != "validation_error" || errorBody["message"] != "agent_key must be a valid agent name" {
					t.Errorf("invalid key error: %s", rr.Body.String())
				}
			}
			if remaining := f.srv.rateLimiters.ChatDM.getLimiter(f.owner.ID).Tokens(); remaining != 30 {
				t.Errorf("invalid key spent a ChatDM token: %v remaining, want 30", remaining)
			}
		})
	}
}

func TestChat_Send_BodyCannotOverrideAuthor(t *testing.T) {
	f := newChatFixture(t)
	path := f.ensure("rook")
	other := mkUser(t, f.srv, "other-chat@example.com")
	rr := f.request("POST", path+"/messages", "rook", map[string]any{"body": "hello", "author_user_id": other.ID, "author_agent_key": "forged", "source": "forged", "seq": 900, "agent_key": "forged", "context_workspace": f.wsSlug})
	f.must(rr, 201, "send")
	m := chatMessage(t, rr)
	if m["author_agent_key"] != "rook" || m["source"] != "cli" || m["seq"] != float64(1) || m["mine"] != true || m["author_is_human"] != false {
		t.Fatalf("credential author: %v", m)
	}
	conv, err := f.srv.store.GetConversationByNumber(models.ConversationActor{UserID: f.owner.ID}, 1)
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.srv.store.ListConversationMessages(models.ConversationActor{UserID: f.owner.ID}, conv.ID, nil, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if page.Messages[0].AuthorUserID != f.owner.ID || page.Messages[0].AuthorAgentKey != "rook" || page.Messages[0].Source != "cli" || page.Messages[0].ContextWorkspaceID == nil || *page.Messages[0].ContextWorkspaceID != f.wsID {
		t.Fatalf("stored author: %+v", page.Messages[0])
	}
	rr = f.request("GET", path+"/messages", "", nil)
	f.must(rr, 200, "human read")
	m = chatObject(t, rr)["messages"].([]any)[0].(map[string]any)
	if m["mine"] != false {
		t.Fatalf("human mine: %v", m)
	}
}
func TestChat_Send_CookieSessionIgnoresAgentHeader(t *testing.T) {
	for _, authorization := range []string{"", "Basic xyz"} {
		t.Run(authorization, func(t *testing.T) {
			f := newChatFixture(t)
			path := f.ensure("rook")
			token, err := f.srv.store.CreateSession(f.owner.ID, "web", "", "", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", path+"/messages", strings.NewReader(`{"body":"human"}`))
			r.AddCookie(&http.Cookie{Name: "pad_session", Value: token})
			r.AddCookie(&http.Cookie{Name: "pad_csrf", Value: strings.Repeat("a", 64)})
			r.Header.Set("X-CSRF-Token", strings.Repeat("a", 64))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Pad-Agent", "rook")
			r.Header.Set("Authorization", authorization)
			rr := httptest.NewRecorder()
			f.srv.ServeHTTP(rr, r)
			if authorization != "" {
				// TASK-4039 r1: TokenAuth rejects a stray Basic header before session auth.
				f.must(rr, 401, "cookie with non-Bearer authorization")
				if chatObject(t, rr)["error"].(map[string]any)["message"] != "Invalid authorization header format" {
					t.Fatal(rr.Body.String())
				}
				return
			}
			f.must(rr, 201, "cookie send")
			m := chatMessage(t, rr)
			if m["author_is_human"] != true || m["mine"] != true || m["author_agent_key"] != "" || m["source"] != "web" {
				t.Fatalf("cookie must author as human: %v", m)
			}
			conv, _ := f.srv.store.GetConversationByNumber(models.ConversationActor{UserID: f.owner.ID}, 1)
			page, err := f.srv.store.ListConversationMessages(models.ConversationActor{UserID: f.owner.ID}, conv.ID, nil, nil, 50)
			if err != nil {
				t.Fatal(err)
			}
			if page.Messages[0].AuthorAgentKey != "" {
				t.Fatalf("stored cookie author = %q, want human", page.Messages[0].AuthorAgentKey)
			}
		})
	}
}
func TestChat_Send_RemoteMCPWithoutNameRefused(t *testing.T) {
	f := newChatFixture(t)
	path := f.ensure("rook")
	for _, inProcess := range []bool{false, true} {
		for _, name := range []string{"", "rook"} {
			r := httptest.NewRequest("POST", path+"/messages", strings.NewReader(`{"body":"mcp"}`))
			if inProcess {
				// TASK-4039 r1: the dispatcher authenticates through context, without a header.
				r = r.WithContext(WithAPITokenAuth(WithCurrentUser(r.Context(), f.owner)))
			} else {
				r.Header.Set("Authorization", "Bearer "+f.ownerTok)
			}
			r.Header.Set("X-Pad-Agent", name)
			r = r.WithContext(WithRemoteMCPCaller(r.Context(), RemoteMCPCaller{}))
			rr := httptest.NewRecorder()
			f.srv.ServeHTTP(rr, r)
			if name == "" {
				f.must(rr, 403, "unnamed MCP")
				if !strings.Contains(rr.Body.String(), "agent_name_required") {
					t.Fatal(rr.Body.String())
				}
			} else {
				f.must(rr, 201, "named MCP")
				if chatMessage(t, rr)["source"] != "cli" {
					t.Fatal(rr.Body.String())
				}
			}
		}
	}
}

// BUG-4055: chat must use the key the real presence stream recorded, even
// when the self-declared header exceeds both the rune and former byte caps.
func TestChat_AgentHeaderTruncatesToPresenceKey(t *testing.T) {
	f := newChatFixture(t)
	f.srv.SetSessionPresence(NewMemorySessionPresence())
	name := strings.Repeat("😀", 70)
	ts := httptest.NewServer(f.srv)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStreamWithHeaders(ctx, t, ts.URL, f.ownerTok, map[string]string{
		"X-Pad-Agent": name, "X-Pad-Worker": presenceWorkerA,
	})
	if ev := waitForWatchEvent(t, ch, 3*time.Second); ev.Type != "connected" {
		t.Fatalf("stream: %q", ev.Type)
	}
	rows := presenceRows(t, f.srv, f.ownerTok)
	if len(rows) != 1 || rows[0]["agent"] != strings.Repeat("😀", 64) {
		t.Fatalf("presence must record the truncated name: %+v", rows)
	}
	presenceKey := rows[0]["agent"].(string)

	rr := f.request("POST", chatBase, name, map[string]any{})
	f.must(rr, 201, "ensure with long header")
	conversation := chatObject(t, rr)["conversation"].(map[string]any)
	if conversation["agent_key"] != presenceKey {
		t.Fatalf("chat key = %v, presence key = %q", conversation["agent_key"], presenceKey)
	}
	path := fmt.Sprintf("%s/%.0f/messages", chatBase, conversation["number"])
	rr = f.request("POST", path, name, map[string]any{"body": "hello"})
	f.must(rr, 201, "send with long header")
	if got := chatMessage(t, rr)["author_agent_key"]; got != presenceKey {
		t.Fatalf("chat author = %v, presence key = %q", got, presenceKey)
	}
}

func TestChat_NonASCII64RuneAgentKeyRoundTrip(t *testing.T) {
	name := strings.Repeat("😀", 64) // 256 bytes; valid without truncation.
	for _, caller := range []string{"human", "agent"} {
		t.Run(caller, func(t *testing.T) {
			f := newChatFixture(t)
			header := ""
			body := map[string]any{"agent_key": name}
			if caller == "agent" {
				header = name
				body = map[string]any{}
			}
			rr := f.request("POST", chatBase, header, body)
			f.must(rr, 201, "ensure 64 rune key")
			conversation := chatObject(t, rr)["conversation"].(map[string]any)
			if conversation["agent_key"] != name {
				t.Fatalf("ensure key = %v, want %q", conversation["agent_key"], name)
			}
			path := fmt.Sprintf("%s/%.0f/messages", chatBase, conversation["number"])
			rr = f.request("POST", path, header, map[string]any{"body": "hello"})
			f.must(rr, 201, "send 64 rune key")
			if got := chatMessage(t, rr)["author_agent_key"]; got != header {
				t.Fatalf("send author = %v, want %q", got, header)
			}
			rr = f.request("GET", path, header, nil)
			f.must(rr, 200, "list messages")
			messages := chatObject(t, rr)["messages"].([]any)
			if len(messages) != 1 {
				t.Fatalf("messages = %v", messages)
			}
			message := messages[0].(map[string]any)
			if message["author_agent_key"] != header || message["body"] != "hello" {
				t.Fatalf("stored message did not round-trip: %v", message)
			}
			rr = f.request("GET", chatBase, header, nil)
			f.must(rr, 200, "list conversations")
			conversations := chatObject(t, rr)["conversations"].([]any)
			if len(conversations) != 1 || conversations[0].(map[string]any)["agent_key"] != name {
				t.Fatalf("stored key did not round-trip byte-identically: %v", conversations)
			}
		})
	}
}

// BUG-4055 r1: an agent whose header truncates must name itself by the
// normalised key, never by the raw header, on both the ensure body and the
// unread filter. Kills: any non-empty agent body refused; the body or the
// filter compared with the raw header instead of the normalised key.
func TestChat_LongHeaderAgent_ExplicitKeyMatchesNormalisedName(t *testing.T) {
	f := newChatFixture(t)
	raw := strings.Repeat("😀", 70)
	key := strings.Repeat("😀", 64)

	rr := f.request("POST", chatBase, raw, map[string]any{"agent_key": key})
	f.must(rr, 201, "agent names itself by the normalised key")
	if got := chatObject(t, rr)["conversation"].(map[string]any)["agent_key"]; got != key {
		t.Fatalf("agent_key = %v, want the 64-rune key", got)
	}
	f.must(f.request("POST", chatBase, raw, map[string]any{"agent_key": key}), 200, "same key again reuses the DM")
	f.must(f.request("POST", chatBase, raw, map[string]any{"agent_key": raw}), 400, "the raw 70-rune name is not the agent's key")

	f.must(f.request("GET", "/api/v1/me/chat/unread?agent="+url.QueryEscape(key), raw, nil), 200, "filter by the normalised key")
	f.must(f.request("GET", "/api/v1/me/chat/unread?agent="+url.QueryEscape(raw), raw, nil), 400, "filter by the raw name")
}

// BUG-4055 / TASK-4059: a human's body agent_key that the normalizer would
// change is refused, not silently changed, and nothing is created. This pins
// the equality gate (normalizeAgentName(key) != key), which a 65-rune key
// reaches first because normalizing truncates it. It does not pin the 64-rune
// bound itself: the handler's rune-count case is shadowed by that gate, so the
// bound is pinned by the store's own table (conversations_test.go "65 runes")
// and the 64-rune non-ASCII round trip above.
func TestChat_Ensure_BodyKeyTheNormalizerWouldChangeIsRefusedWithoutCreation(t *testing.T) {
	f := newChatFixture(t)
	// ASCII keeps this under the former byte cap, so silently truncating
	// the explicit identifier cannot hide behind an unrelated byte refusal.
	rr := f.request("POST", chatBase, "", map[string]any{"agent_key": strings.Repeat("x", 65)})
	if rr.Code != 400 {
		t.Errorf("ensure = %d %s, want 400", rr.Code, rr.Body.String())
	} else {
		e := chatObject(t, rr)["error"].(map[string]any)
		if e["code"] != "validation_error" {
			t.Errorf("error code = %v, want validation_error", e["code"])
		}
		// The equality gate's own message, so a refusal by any later case
		// (the shadowed rune bound included) does not satisfy this test.
		if e["message"] != "agent_key must be a valid agent name" {
			t.Errorf("refused by %q, want the equality gate's refusal", e["message"])
		}
	}
	rows, err := f.srv.store.ListConversations(models.ConversationActor{UserID: f.owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("refused key created conversations: %+v", rows)
	}
}
func TestChat_RateLimit_20PerMinutePerAgentPerConversation(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "")
	f := newChatFixture(t)
	path := f.ensure("rook")
	for i := 0; i < 20; i++ {
		f.must(f.request("POST", path+"/messages", "rook", map[string]any{"body": "agent"}), 201, "agent send")
	}
	rr := f.request("POST", path+"/messages", "rook", map[string]any{"body": "agent"})
	f.must(rr, 429, "21st agent send")
	if rr.Header().Get("X-RateLimit-Limit") != "20" || rr.Header().Get("Retry-After") != "3" {
		t.Fatalf("chat limit headers: %v", rr.Header())
	}

	for i := 0; i < 21; i++ {
		f.must(f.request("POST", path+"/messages", "", map[string]any{"body": "human"}), 201, "human sends bypass chat bucket")
	}

	rl := f.srv.rateLimiters.ChatSend
	rl.mu.Lock()
	before := len(rl.limiters)
	rl.mu.Unlock()
	if before != 1 {
		t.Fatalf("only agent sends may allocate a bucket: %d", before)
	}
	f.must(f.request("POST", path+"/messages", "outsider", map[string]any{"body": "no"}), 404, "nonmember")
	rl.mu.Lock()
	after := len(rl.limiters)
	rl.mu.Unlock()
	if before != after {
		t.Fatalf("nonmember allocated a chat bucket: %d -> %d", before, after)
	}
	p2 := f.ensure("owl")
	f.must(f.request("POST", p2+"/messages", "owl", map[string]any{"body": "separate"}), 201, "separate conversation")

}
func TestChat_Ensure_ReuseIsUncharged(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "")
	f := newChatFixture(t)
	f.ensure("rook")
	for i := 0; i < 40; i++ {
		// TASK-4039 r2: all agents share the owner's creation bucket.
		rr := f.request("POST", chatBase, "rook", map[string]any{})
		f.must(rr, 200, fmt.Sprintf("reuse %d", i+1))
		if chatObject(t, rr)["created"] != false {
			t.Fatal("reuse reported created")
		}
	}
	f.must(f.request("POST", chatBase, "owl", map[string]any{}), 201, "create after reuses")
}

func TestChat_Ensure_CreateBucketStillLimits(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "")
	f := newChatFixture(t)
	for _, key := range []string{"", strings.Repeat("x", 129), "bad\nkey"} {
		f.must(f.request("POST", chatBase, "", map[string]any{"agent_key": key}), 400, "invalid key is uncharged")
	}
	for i := 0; i < 30; i++ {
		f.must(f.request("POST", chatBase, "", map[string]any{"agent_key": fmt.Sprintf("agent-%d", i)}), 201, "create DM")
	}
	rr := f.request("POST", chatBase, "", map[string]any{"agent_key": "overflow"})
	f.must(rr, 429, "31st DM")
	if rr.Header().Get("Retry-After") != "120" {
		t.Fatalf("DM retry: %v", rr.Header())
	}
	f.must(f.request("POST", chatBase, "", map[string]any{}), 400, "empty key with exhausted bucket")
	f.must(f.request("POST", chatBase, "agent-0", map[string]any{}), 200, "reuse with exhausted bucket")
	other := mkUser(t, f.srv, "dm-other-chat@example.com")
	rr = rawReq(f.srv, "POST", chatBase, f.token(other), "", `{"agent_key":"rook"}`)
	f.must(rr, 201, "DM bucket is per user")

}

// TASK-4039: admission must run before any handler-specific parsing.
func chatDoors(s *Server) []http.HandlerFunc {
	return []http.HandlerFunc{s.handleListChatConversations, s.handleEnsureChatConversation, s.handleGetChatConversation, s.handleListChatMessages, s.handleSendChatMessage, s.handleDeleteChatMessage, s.handleMarkChatRead, s.handleChatUnread}
}
func TestChat_AppPrincipalRefused(t *testing.T) {
	f := newChatFixture(t)
	for i, h := range chatDoors(f.srv) {
		for _, kind := range []string{"context", "app-user", "none"} {
			r := httptest.NewRequest("POST", chatBase, nil)
			ctx := r.Context()
			switch kind {
			case "context":
				ctx = context.WithValue(WithCurrentUser(ctx, f.owner), appCtxKey{}, &appContext{})
			case "app-user":
				u := *f.owner
				u.Kind = "app"
				ctx = WithCurrentUser(ctx, &u)
			}
			rr := httptest.NewRecorder()
			h(rr, r.WithContext(ctx))
			want := 403
			if kind == "none" {
				want = 401
			}
			f.must(rr, want, fmt.Sprintf("door %d %s", i, kind))
		}
	}
}

func TestChat_List_InboxRowsAndUnreadTotal(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	f.ensure("owl")
	f.must(f.request("POST", p+"/messages", "rook", map[string]any{"body": "first line\nsecret second line"}), 201, "agent send")
	for _, agent := range []string{"", "rook"} {
		rr := f.request("GET", chatBase, agent, nil)
		f.must(rr, 200, "inbox")
		out := chatObject(t, rr)
		rows := out["conversations"].([]any)
		want := 2
		if agent != "" {
			want = 1
		}
		if len(rows) != want {
			t.Fatalf("inbox %s: %v", agent, out)
		}
		for _, r := range rows {
			if last, ok := r.(map[string]any)["last_message"].(map[string]any); ok {
				if _, ok := last["body"]; ok {
					t.Fatal(last)
				}
				if last["excerpt"] != "first line" {
					t.Fatal(last)
				}
			}
		}
		if agent == "" && out["unread_total"] != float64(1) {
			t.Fatal(out)
		}
	}
}
func TestChat_GetByNumber_HumanAndAgent(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	for _, agent := range []string{"", "rook"} {
		rr := f.request("GET", p, agent, nil)
		f.must(rr, 200, "detail")
		out := chatObject(t, rr)
		if out["agent_key"] != "rook" || out["read_seq"] != float64(0) || out["unread"] != float64(0) {
			t.Fatal(out)
		}
	}
	f.must(f.request("GET", p, "owl", nil), 404, "other agent")
	for _, n := range []string{"abc", "0", "-1", "99999999999999999999"} {
		f.must(f.request("GET", chatBase+"/"+n, "", nil), 404, "invalid number")
	}
}
func TestChat_Messages_PaginationTombstonesAndLimits(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	rr := f.request("GET", p+"/messages", "", nil)
	f.must(rr, 200, "empty")
	if rows, ok := chatObject(t, rr)["messages"].([]any); !ok || len(rows) != 0 {
		t.Fatal(rr.Body.String())
	}
	c, _ := f.srv.store.GetConversationByNumber(models.ConversationActor{UserID: f.owner.ID}, 1)
	for i := 0; i < 205; i++ {
		_, err := f.srv.store.SendConversationMessage(models.ConversationSend{ConversationID: c.ID, Author: models.ConversationActor{UserID: f.owner.ID}, Body: fmt.Sprint(i), Source: "cli"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.srv.store.DeleteConversationMessage(models.ConversationActor{UserID: f.owner.ID}, c.ID, 203); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		q                     string
		count, oldest, newest int
		more                  bool
	}{{"", 50, 156, 205, true}, {"?limit=999", 200, 6, 205, true}, {"?before_seq=4&limit=2", 2, 2, 3, true}, {"?after_seq=203&limit=2", 2, 204, 205, false}, {"?after_seq=1&limit=2", 2, 2, 3, true}} {
		rr = f.request("GET", p+"/messages"+tc.q, "", nil)
		f.must(rr, 200, "page")
		out := chatObject(t, rr)
		if len(out["messages"].([]any)) != tc.count || out["oldest_seq"] != float64(tc.oldest) || out["newest_seq"] != float64(tc.newest) || out["last_seq"] != float64(205) || out["has_more"] != tc.more {
			t.Fatalf("page %s: %s", tc.q, rr.Body.String())
		}
		if tc.q == "" {
			m := out["messages"].([]any)[47].(map[string]any)
			if m["seq"] != float64(203) || m["deleted"] != true || m["body"] != "" {
				t.Fatal(m)
			}
		}
	}
	for _, q := range []string{"?limit=-1", "?limit=wat", "?before_seq=-1", "?after_seq=no", "?before_seq=1&after_seq=2", "?before_seq=", "?limit=99999999999999999999"} {
		f.must(f.request("GET", p+"/messages"+q, "", nil), 400, q)
	}
}
func TestChat_Send_OpKeyReplayOverHTTP(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	rr := f.request("POST", p+"/messages", "rook", map[string]any{"body": "first", "op_key": "once"})
	f.must(rr, 201, "first")
	id := chatMessage(t, rr)["id"]
	if chatObject(t, rr)["replayed"] != false {
		t.Fatal(rr.Body.String())
	}
	for _, deleted := range []bool{false, true} {
		if deleted {
			f.must(f.request("DELETE", p+"/messages/1", "rook", nil), 200, "delete")
		}
		rr = f.request("POST", p+"/messages", "rook", map[string]any{"body": "replacement", "op_key": "once"})
		f.must(rr, 200, "replay")
		m := chatMessage(t, rr)
		if chatObject(t, rr)["replayed"] != true || m["id"] != id || m["seq"] != float64(1) || m["deleted"] != deleted {
			t.Fatal(rr.Body.String())
		}
		if !deleted && m["body"] != "first" {
			t.Fatal(m)
		}
	}
	rr = f.request("GET", p, "", nil)
	if chatObject(t, rr)["last_seq"] != float64(1) {
		t.Fatal(rr.Body.String())
	}
}
func TestChat_Send_ReplyAndQuote(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": strings.Repeat("界", 110) + "\nnever quote this"}), 201, "original")
	rr := f.request("POST", p+"/messages", "rook", map[string]any{"body": "reply", "reply_to_seq": 1})
	f.must(rr, 201, "reply")
	q := chatMessage(t, rr)["reply_to"].(map[string]any)
	excerpt := q["excerpt"].(string)
	if len([]rune(excerpt)) != 100 || !strings.HasSuffix(excerpt, "...") || strings.Contains(excerpt, "never") {
		t.Fatal(q)
	}
	f.must(f.request("DELETE", p+"/messages/1", "", nil), 200, "delete original")
	rr = f.request("GET", p+"/messages", "", nil)
	q = chatObject(t, rr)["messages"].([]any)[1].(map[string]any)["reply_to"].(map[string]any)
	if q["deleted"] != true || q["excerpt"] != "" {
		t.Fatal(q)
	}
}
func TestChat_Send_SentinelStatusTable(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "gone"}), 201, "seed")
	f.must(f.request("DELETE", p+"/messages/1", "", nil), 200, "delete")
	for _, tc := range []struct {
		body   any
		status int
		code   string
	}{{map[string]any{"body": ""}, 400, "validation_error"}, {map[string]any{"body": strings.Repeat("x", 65537)}, 400, "validation_error"}, {map[string]any{"body": "reply", "reply_to_seq": 0}, 400, "validation_error"}, {map[string]any{"body": "reply", "reply_to_seq": 90}, 422, "reply_target_not_found"}, {map[string]any{"body": "reply", "reply_to_seq": 1}, 409, "reply_target_deleted"}} {
		rr := f.request("POST", p+"/messages", "", tc.body)
		f.must(rr, tc.status, tc.code)
		if chatObject(t, rr)["error"].(map[string]any)["code"] != tc.code {
			t.Fatal(rr.Body.String())
		}
	}
}
func TestChat_Send_DoesNotMoveMarkers(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	for _, author := range []string{"", "rook"} {
		f.must(f.request("POST", p+"/messages", author, map[string]any{"body": "hi"}), 201, "send")
	}
	for _, reader := range []string{"", "rook"} {
		rr := f.request("GET", p, reader, nil)
		f.must(rr, 200, "detail")
		if chatObject(t, rr)["read_seq"] != float64(0) {
			t.Fatal(rr.Body.String())
		}
		rr = f.request("GET", "/api/v1/me/chat/unread", reader, nil)
		f.must(rr, 200, "unread")
		rows := chatObject(t, rr)["conversations"].([]any)
		if len(rows) != 1 {
			t.Fatal(rr.Body.String())
		}
		row := rows[0].(map[string]any)
		if row["read_seq"] != float64(0) || row["delivered_seq"] != float64(0) || row["unread"] != float64(1) {
			t.Fatal(row)
		}
	}
}
func TestChat_Delete_AuthorAndOwnerRules(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	for _, author := range []string{"", "rook", "rook"} {
		f.must(f.request("POST", p+"/messages", author, map[string]any{"body": "erase me"}), 201, "seed")
	}
	f.must(f.request("DELETE", p+"/messages/1", "rook", nil), 403, "agent deleting human")
	for i, author := range []string{"", "", "rook"} {
		path := fmt.Sprintf("%s/messages/%d", p, i+1)
		for _, want := range []bool{true, false} {
			rr := f.request("DELETE", path, author, nil)
			f.must(rr, 200, "delete")
			out := chatObject(t, rr)
			if out["deleted"] != want || chatMessage(t, rr)["body"] != "" || chatMessage(t, rr)["deleted"] != true {
				t.Fatal(out)
			}
		}
	}
	for _, seq := range []string{"999", "abc", "0", "99999999999999999999"} {
		f.must(f.request("DELETE", p+"/messages/"+seq, "", nil), 404, "missing seq")
	}
	rr := f.request("GET", p+"/messages", "", nil)
	if strings.Contains(rr.Body.String(), "erase me") {
		t.Fatal(rr.Body.String())
	}
}
func TestChat_MarkRead_DisplayedSeq(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	for i := 0; i < 3; i++ {
		f.must(f.request("POST", p+"/messages", "rook", map[string]any{"body": "hi"}), 201, "seed")
	}
	for _, tc := range []struct{ seq, want int }{{2, 2}, {1, 2}, {999, 3}} {
		rr := f.request("POST", p+"/read", "", map[string]any{"seq": tc.seq})
		f.must(rr, 200, "mark read")
		if chatObject(t, rr)["read_seq"] != float64(tc.want) {
			t.Fatal(rr.Body.String())
		}
		rr = f.request("GET", p, "", nil)
		if chatObject(t, rr)["unread"] != float64(3-tc.want) {
			t.Fatal(rr.Body.String())
		}
	}
	for _, body := range []any{map[string]any{}, map[string]any{"seq": -1}, map[string]any{"seq": nil}} {
		f.must(f.request("POST", p+"/read", "", body), 400, "bad read seq")
	}
}
func TestChat_Unread_AgentFilterAndMismatch(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	f.ensure("owl")
	f.must(f.request("POST", p+"/messages", "rook", map[string]any{"body": "hi"}), 201, "send")
	for _, tc := range []struct {
		agent, q string
		total    int
	}{{"", "", 1}, {"", "?agent=rook", 1}, {"", "?agent=owl", 0}, {"rook", "?agent=rook", 0}} {
		rr := f.request("GET", "/api/v1/me/chat/unread"+tc.q, tc.agent, nil)
		f.must(rr, 200, "unread filter")
		if chatObject(t, rr)["total"] != float64(tc.total) {
			t.Fatal(rr.Body.String())
		}
	}
	f.must(f.request("GET", "/api/v1/me/chat/unread?agent=owl", "rook", nil), 400, "mismatched agent")
}
func chatRouteCases() []struct{ method, path, body string } {
	return []struct{ method, path, body string }{{"GET", chatBase, ""}, {"POST", chatBase, `{"agent_key":"rook"}`}, {"GET", chatBase + "/1", ""}, {"GET", chatBase + "/1/messages", ""}, {"POST", chatBase + "/1/messages", `{"body":"hi"}`}, {"DELETE", chatBase + "/1/messages/1", ""}, {"POST", chatBase + "/1/read", `{"seq":0}`}, {"POST", chatBase + "/1/delivered", `{"seq":0}`}, {"GET", "/api/v1/me/chat/unread", ""}}
}
func TestChat_NonMemberGets404Everywhere(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	other := mkUser(t, f.srv, "outsider-chat@example.com")
	tok := f.token(other)
	for _, tc := range chatRouteCases()[2:8] {
		rr := rawReq(f.srv, tc.method, tc.path, tok, "", tc.body)
		f.must(rr, 404, "other user")
		missing := rawReq(f.srv, tc.method, strings.Replace(tc.path, "/1", "/999", 1), tok, "", tc.body)
		if rr.Body.String() != missing.Body.String() {
			t.Fatalf("existence oracle: %s / %s", rr.Body.String(), missing.Body.String())
		}
		f.must(f.request(tc.method, tc.path, "owl", json.RawMessage(tcBody(tc.body))), 404, "other agent")
	}
	c, _, err := f.srv.store.EnsureAgentDM(other.ID, "owl")
	if err != nil {
		t.Fatal(err)
	}
	rr := rawReq(f.srv, "GET", p, tok, "", "")
	f.must(rr, 200, "independent namespace")
	if chatObject(t, rr)["id"] != c.ID {
		t.Fatal(rr.Body.String())
	}
}
func tcBody(s string) string {
	if s == "" {
		return "null"
	}
	return s
}
func TestChat_UserlessLegacyTokenRefused(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	f := newChatFixture(t)
	tok, err := f.srv.store.CreateAPIToken(f.owner.ID, models.APITokenCreate{Name: "legacy-chat", WorkspaceID: f.wsID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.srv.store.DB().Exec(`UPDATE api_tokens SET user_id=NULL WHERE id=?`, tok.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range chatRouteCases() {
		rr := rawReq(f.srv, tc.method, tc.path, tok.Token, "", tc.body)
		f.must(rr, 403, "legacy")
		if !strings.Contains(rr.Body.String(), "legacy_token_not_allowed") {
			t.Fatal(rr.Body.String())
		}
	}
}
func TestChat_PinnedPATRefused(t *testing.T) {
	f := newChatFixture(t)
	tok, err := f.srv.store.CreateAPIToken(f.owner.ID, models.APITokenCreate{Name: "pin-chat", WorkspaceID: f.wsID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range chatRouteCases() {
		rr := rawReq(f.srv, tc.method, tc.path, tok.Token, "", tc.body)
		f.must(rr, 403, "pin")
		if !strings.Contains(rr.Body.String(), "token_workspace_pinned") {
			t.Fatal(rr.Body.String())
		}
	}
}
func TestChat_ReadScopedPATCanReadNotWrite(t *testing.T) {
	f := newChatFixture(t)
	f.ensure("rook")
	tok, err := f.srv.store.CreateAPIToken(f.owner.ID, models.APITokenCreate{Name: "read-chat", Scopes: `["read"]`}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range chatRouteCases() {
		rr := rawReq(f.srv, tc.method, tc.path, tok.Token, "", tc.body)
		want := 403
		if tc.method == "GET" {
			want = 200
		}
		f.must(rr, want, "read scope")
	}
}
func TestChat_FreshInstallNoUser_401(t *testing.T) {
	srv := testServer(t)
	for _, tc := range chatRouteCases() {
		rr := rawReq(srv, tc.method, tc.path, "", "", tc.body)
		if rr.Code != 401 {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

// TASK-4039: sentinel details are static; unexpected store text never reaches HTTP.
func TestChat_WriteChatError_SentinelMapping(t *testing.T) {
	for _, tc := range []struct {
		err           error
		status        int
		code, message string
	}{
		{store.ErrConversationNotFound, 404, "not_found", "Conversation not found"},
		{fmt.Errorf("%w: body is required", store.ErrInvalidConversationInput), 400, "validation_error", "invalid conversation input: body is required"},
		{store.ErrConversationForbidden, 403, "forbidden", "You can only delete your own messages"},
		{store.ErrReplyTargetNotFound, 422, "reply_target_not_found", "Reply target not found"},
		{store.ErrReplyTargetDeleted, 409, "reply_target_deleted", "Reply target was deleted"},
		{errors.New("private SQL diagnostic"), 500, "internal_error", ""},
	} {
		t.Run(tc.code, func(t *testing.T) {
			rr := httptest.NewRecorder()
			writeChatError(rr, tc.err)
			if rr.Code != tc.status {
				t.Fatalf("status: %d %s", rr.Code, rr.Body.String())
			}
			e := chatObject(t, rr)["error"].(map[string]any)
			if e["code"] != tc.code || (tc.message != "" && e["message"] != tc.message) {
				t.Fatal(e)
			}
			if strings.Contains(rr.Body.String(), "private SQL diagnostic") || strings.Contains(rr.Body.String(), store.ErrConversationForbidden.Error()) {
				t.Fatal(rr.Body.String())
			}
		})
	}
}

func TestChat_Send_CreatesWithCredentialAuthor(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	for _, agent := range []string{"", "rook"} {
		rr := f.request("POST", p+"/messages", agent, map[string]any{"body": "hi"})
		f.must(rr, 201, "send")
		m := chatMessage(t, rr)
		if m["mine"] != true || m["author_is_human"] != (agent == "") || m["author_agent_key"] != agent {
			t.Fatal(m)
		}
	}
	rr := f.request("GET", p+"/messages", "rook", nil)
	f.must(rr, 200, "agent page")
	rows := chatObject(t, rr)["messages"].([]any)
	if rows[0].(map[string]any)["mine"] != false || rows[1].(map[string]any)["mine"] != true {
		t.Fatal(rows)
	}
}
func TestChat_NonMemberAdminCookieGets404(t *testing.T) {
	f := newChatFixture(t)
	f.ensure("rook")
	admin := mkUserRole(t, f.srv, "admin-chat@example.com", "admin")
	token, err := f.srv.store.CreateSession(admin.ID, "web", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range chatRouteCases()[2:8] {
		rr := rawReq(f.srv, tc.method, tc.path, "", token, tc.body)
		f.must(rr, 404, "admin nonmember")
		missing := rawReq(f.srv, tc.method, strings.Replace(tc.path, "/1", "/999", 1), "", token, tc.body)
		if rr.Body.String() != missing.Body.String() {
			t.Fatalf("admin existence oracle: %s / %s", rr.Body.String(), missing.Body.String())
		}
	}
}

// TASK-4039 r1: an identical retry survives each kind of context access loss.
func TestChat_Send_OpKeyReplayAfterContextMembershipRemoved(t *testing.T) {
	for _, loss := range []string{"member removed", "workspace soft-deleted", "token access lost"} {
		t.Run(loss, func(t *testing.T) {
			f := newChatEnrichFixture(t)
			p := f.ensure("rook")
			denial := f.request("POST", p+"/messages", "", map[string]any{"body": "hello", "context_workspace": "unknown-workspace"})
			f.must(denial, 400, "unknown context")
			body := map[string]any{"body": "[[TASK-1]] stored with context", "op_key": "with-context", "context_workspace": f.wsID}
			first := f.request("POST", p+"/messages", "rook", body)
			f.must(first, 201, "send with context")
			firstMessage := chatMessage(t, first)
			switch loss {
			case "member removed":
				if err := f.srv.store.RemoveWorkspaceMember(f.wsID, f.owner.ID); err != nil {
					t.Fatal(err)
				}
			case "workspace soft-deleted":
				if err := f.srv.store.DeleteWorkspace(f.wsSlug); err != nil {
					t.Fatal(err)
				}
			}
			retry := func() *httptest.ResponseRecorder {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest("POST", p+"/messages", strings.NewReader(string(raw)))
				r.Header.Set("Authorization", "Bearer "+f.ownerTok)
				r.Header.Set("X-Pad-Agent", "rook")
				r.Header.Set("Content-Type", "application/json")
				if loss == "token access lost" {
					r = r.WithContext(WithTokenAllowedWorkspaces(r.Context(), []string{"another-workspace"}))
				}
				rr := httptest.NewRecorder()
				f.srv.ServeHTTP(rr, r)
				return rr
			}
			rr := retry()
			f.must(rr, 200, "identical replay after context loss")
			m := chatMessage(t, rr)
			if chatObject(t, rr)["replayed"] != true || m["id"] != firstMessage["id"] || m["seq"] != firstMessage["seq"] || m["body"] != body["body"] {
				t.Fatal(rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), f.wsID) || strings.Contains(rr.Body.String(), f.wsSlug) || strings.Contains(rr.Body.String(), "context_workspace") {
				t.Fatalf("lost context leaked: %s", rr.Body.String())
			}
			if refs := chatRefs(t, rr); len(refs) != 1 || refs[0]["state"] != "unresolved" || refs[0]["workspace"] != "" {
				t.Fatalf("replay refs: %v", refs)
			}
			for _, key := range []string{"new-key", ""} {
				body["op_key"] = key
				rr = retry()
				f.must(rr, 400, "lost context with no prior op_key")
				if rr.Body.String() != denial.Body.String() {
					t.Fatalf("context denial changed: %s / %s", rr.Body.String(), denial.Body.String())
				}
			}
		})
	}
}

// TASK-4039 r1: both replay paths spend exactly one token and share exhaustion.
func TestChat_RateLimit_ReplayAfterContextLoss(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "")
	for _, sends := range []int{19, 20} {
		for _, lost := range []bool{false, true} {
			t.Run(fmt.Sprintf("sends=%d/lost=%v", sends, lost), func(t *testing.T) {
				f := newChatEnrichFixture(t)
				p := f.ensure("rook")
				body := map[string]any{"body": "original", "op_key": "once", "context_workspace": f.wsID}
				f.must(f.request("POST", p+"/messages", "rook", body), 201, "original")
				for i := 1; i < sends; i++ {
					f.must(f.request("POST", p+"/messages", "rook", map[string]any{"body": "fill"}), 201, "fill bucket")
				}
				if lost {
					if err := f.srv.store.RemoveWorkspaceMember(f.wsID, f.owner.ID); err != nil {
						t.Fatal(err)
					}
				}
				rr := f.request("POST", p+"/messages", "rook", body)
				want := 200
				if sends == 20 {
					want = 429
				}
				f.must(rr, want, "replay bucket parity")
				if want == 200 && chatObject(t, rr)["replayed"] != true {
					t.Fatal(rr.Body.String())
				}
				rr = f.request("POST", p+"/messages", "rook", map[string]any{"body": "original", "op_key": "once"})
				f.must(rr, 429, "normal replay after bucket exhausted")
				if rr.Header().Get("X-RateLimit-Remaining") != "0" {
					t.Fatalf("remaining: %v", rr.Header())
				}
			})
		}
	}
}

func TestChat_EnsureDM_EmptyBody(t *testing.T) {
	for _, agent := range []string{"rook", ""} {
		t.Run(agent, func(t *testing.T) {
			f := newChatFixture(t)
			r := httptest.NewRequest("POST", chatBase, nil)
			r.Header.Set("Authorization", "Bearer "+f.ownerTok)
			r.Header.Set("X-Pad-Agent", agent)
			rr := httptest.NewRecorder()
			f.srv.ServeHTTP(rr, r)
			if agent == "" {
				f.must(rr, 400, "human still requires agent_key")
				if chatObject(t, rr)["error"].(map[string]any)["message"] != "invalid conversation input: agent key is required" {
					t.Fatal(rr.Body.String())
				}
			} else {
				f.must(rr, 201, "agent empty body")
				c := chatObject(t, rr)["conversation"].(map[string]any)
				if c["agent_key"] != agent || chatObject(t, rr)["created"] != true {
					t.Fatal(rr.Body.String())
				}
			}
		})
	}
}

// BUG-4092 / TASK-4044: the web client re-applies this normalizer to agent
// names (web/src/lib/chat/presence.ts, its test "trims exactly what Go
// TrimSpace trims"). These are the same five inputs. Test-only: a change to
// the Go side must fail here so the web copy does not drift silently.
func TestNormalizeAgentNameTrimParity(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\uFEFFcodex\uFEFF", "\uFEFFcodex\uFEFF"}, // Go keeps U+FEFF; JS trim() would not
		{"  codex\u3000", "codex"},
		{"\u0085 codex", "codex"},
		{"\u200Bcodex", "\u200Bcodex"}, // ZWSP is not White_Space
		{strings.Repeat("a", 63) + " b", strings.Repeat("a", 63)},
	}
	for _, c := range cases {
		if got := normalizeAgentName(c.in); got != c.want {
			t.Errorf("normalizeAgentName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
