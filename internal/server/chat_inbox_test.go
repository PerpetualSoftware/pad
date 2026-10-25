package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TASK-4044: the chat inbox. A cookie stream receives the ref-only notices
// addressed to its own user and nothing else.

func inboxNotice(user string) watchevents.Notification {
	return watchevents.Notification{Kind: watchevents.KindChat, ConversationID: "conv", ConversationNumber: 1, MessageID: "msg", MessageSeq: 1, TargetUserID: user}
}

func TestChatInboxDeliveryPredicate(t *testing.T) {
	deliver := func(p watchStreamPrefs, user string, n watchevents.Notification) bool {
		return watchStreamDelivers(map[string]string{"": ""}, watchAccessVisibility{}, user, "", p, n)
	}
	inbox := watchStreamPrefs{chatInbox: true}
	agent := watchStreamPrefs{chat: true, armed: true, agentKey: "rook"}
	for _, tc := range []struct {
		name string
		p    watchStreamPrefs
		user string
		n    watchevents.Notification
		want bool
	}{
		{"baseline", inbox, "u", inboxNotice("u"), true},
		{"chat_inbox false", watchStreamPrefs{}, "u", inboxNotice("u"), false},
		{"chat+armed without inbox", watchStreamPrefs{chat: true, armed: true}, "u", inboxNotice("u"), false},
		{"another user", inbox, "u", inboxNotice("other"), false},
		{"empty user", inbox, "", inboxNotice(""), false},
		{"empty stream user", inbox, "", inboxNotice("u"), false},
		{"agent notice on inbox-only stream", inbox, "u", chatNotice(), false},
		{"agent delivery unchanged", agent, "u", chatNotice(), true},
		{"agent stream gets no inbox notice", agent, "u", inboxNotice("u"), false},
		{"agent stream with inbox, own agent notice", watchStreamPrefs{chat: true, armed: true, agentKey: "rook", chatInbox: true}, "u", chatNotice(), true},
		{"agent stream with inbox, other agent", watchStreamPrefs{chat: true, armed: true, agentKey: "owl", chatInbox: true}, "u", chatNotice(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := deliver(tc.p, tc.user, tc.n); got != tc.want {
				t.Fatalf("delivery=%v want %v", got, tc.want)
			}
		})
	}
}

// No app principal reaches this stream by a non-bearer path (its routes are
// all behind the cookie or token middleware, and the revalidation tick denies
// a bot), so the app clauses of the pref are covered here, directly.
func TestChatInboxPrefHonouredOnlyForPersonCookie(t *testing.T) {
	person := &models.User{ID: "u"}
	bot := &models.User{ID: "b", Kind: models.UserKindApp}
	for _, tc := range []struct {
		name   string
		query  string
		header string
		user   *models.User
		app    bool
		want   bool
	}{
		{"cookie person", "chat_inbox=true", "", person, false, true},
		{"param absent", "", "", person, false, false},
		{"param not true", "chat_inbox=1", "", person, false, false},
		{"bearer", "chat_inbox=true", "Bearer pad_x", person, false, false},
		{"app principal", "chat_inbox=true", "", bot, false, false},
		{"app context", "chat_inbox=true", "", person, true, false},
		{"no user", "chat_inbox=true", "", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/v1/events/stream?"+tc.query, nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			if tc.app {
				r = r.WithContext(context.WithValue(r.Context(), appCtxKey{}, &appContext{}))
			}
			if got := chatInboxPref(r, tc.user); got != tc.want {
				t.Fatalf("pref=%v want %v", got, tc.want)
			}
		})
	}
}

// An agent notice published alongside a human one for the same message keeps
// its excerpt and reply seq (the monitor's line needs them); the human notice
// has neither, and no agent key.
func TestChatPublishInboxNoticeIsRefOnlyAgentNoticeKeepsExcerpt(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	f.must(f.request("POST", p+"/messages", "rook", map[string]any{"body": "first"}), 201, "agent send")
	mark := f.mark()
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "a reply\nSECOND-LINE", "reply_to_seq": 1}), 201, "human reply")
	ns := f.srv.watchEvents.EventsSince(mark)
	if len(ns) != 2 {
		t.Fatalf("notices=%+v", ns)
	}
	agent := chatNoticeFor(t, ns, "rook")
	if agent.Excerpt != "a reply" || agent.ReplyToSeq == nil || *agent.ReplyToSeq != 1 {
		t.Fatalf("agent notice lost its excerpt or reply seq: %+v", agent)
	}
	human := chatNoticeFor(t, ns, "")
	if human.TargetUserID != f.owner.ID || human.Excerpt != "" || human.ReplyToSeq != nil || human.TargetAgentKey != "" || human.MessageSeq != agent.MessageSeq {
		t.Fatalf("inbox notice is not ref-only: %+v", human)
	}
	b, _ := json.Marshal(watchEventPayloadFor(nil, human))
	for _, bad := range []string{"excerpt", "reply_to_seq", "target_agent_key", "a reply", "SECOND-LINE"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("inbox wire carries %q: %s", bad, b)
		}
	}
}

func TestChatPublishInboxDeleteDedupesAuthor(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "mine"}), 201, "human send")
	mark := f.mark()
	f.must(f.request("DELETE", p+"/messages/1", "", nil), 200, "delete")
	ns := f.srv.watchEvents.EventsSince(mark)
	if len(ns) != 2 {
		t.Fatalf("notices=%+v", ns)
	}
	if h := chatNoticeFor(t, ns, ""); !h.ChatDeleted || h.TargetUserID != f.owner.ID {
		t.Fatalf("bad retraction inbox notice: %+v", h)
	}
}

// chatEventsWithin collects the chat notifications that arrive on ch within d.
func chatEventsWithin(ch <-chan watchSSEEvent, d time.Duration) []watchEventPayload {
	var out []watchEventPayload
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			var p watchEventPayload
			if json.Unmarshal([]byte(ev.Data), &p) == nil && p.Kind == watchevents.KindChat {
				out = append(out, p)
			}
		case <-deadline:
			return out
		}
	}
}

func TestChatInboxLiveStreams(t *testing.T) {
	f := newChatFixture(t)
	h := httptest.NewServer(f.srv)
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cookie := func(userID string) map[string]string {
		tok, err := f.srv.store.CreateSession(userID, "test", "127.0.0.1", testSessionUA, webSessionTTL)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]string{"Cookie": sessionCookieName(f.srv.secureCookies) + "=" + tok, "User-Agent": testSessionUA}
	}
	other := mkUser(t, f.srv, "other@example.com")

	inbox := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, "", cookie(f.owner.ID), "chat_inbox=true")
	plain := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, "", cookie(f.owner.ID), "")
	everything := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, "", cookie(f.owner.ID), "access=true&armed=true&chat=true&chat_inbox=true")
	otherInbox := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, "", cookie(other.ID), "chat_inbox=true")
	pat := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, f.ownerTok, nil, "armed=true&chat=true&chat_inbox=true")
	rook := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, f.ownerTok, map[string]string{"X-Pad-Agent": "rook"}, "armed=true&chat=true")
	for _, ch := range []<-chan watchSSEEvent{inbox, plain, everything, otherInbox, pat, rook} {
		waitForWatchEvent(t, ch, 2*time.Second)
	}

	p := f.ensure("rook")
	const window = 400 * time.Millisecond

	// An agent sends to its owner.
	f.must(f.request("POST", p+"/messages", "rook", map[string]any{"body": "SECRET-BODY from rook"}), 201, "agent send")
	got := chatEventsWithin(inbox, window)
	if len(got) != 1 || got[0].MessageSeq != 1 || got[0].Excerpt != "" || got[0].ReplyToSeq != nil || got[0].TargetAgentKey != "" || got[0].AuthorAgentKey != "rook" || got[0].AuthorIsHuman == nil || *got[0].AuthorIsHuman {
		t.Fatalf("inbox stream, agent send: %+v", got)
	}
	if got := chatEventsWithin(everything, window); len(got) != 1 || got[0].TargetAgentKey != "" {
		t.Fatalf("access+armed+chat+inbox stream, agent send: %+v", got)
	}
	for name, ch := range map[string]<-chan watchSSEEvent{"no param": plain, "other user": otherInbox, "pat": pat, "agent bearer": rook} {
		if got := chatEventsWithin(ch, 100*time.Millisecond); len(got) != 0 {
			t.Fatalf("%s stream received an agent-authored inbox notice: %+v", name, got)
		}
	}

	// A person sends: their own tabs see it, and the agent's stream gets exactly its one event.
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "hello"}), 201, "human send")
	got = chatEventsWithin(inbox, window)
	if len(got) != 1 || got[0].MessageSeq != 2 || got[0].AuthorIsHuman == nil || !*got[0].AuthorIsHuman {
		t.Fatalf("inbox stream, human send: %+v", got)
	}
	if got := chatEventsWithin(everything, window); len(got) != 1 || got[0].TargetAgentKey != "" {
		t.Fatalf("access+armed+chat+inbox stream, human send: %+v", got)
	}
	if got := chatEventsWithin(rook, window); len(got) != 1 || got[0].TargetAgentKey != "rook" || got[0].Excerpt != "hello" {
		t.Fatalf("agent bearer stream, human send: %+v", got)
	}
	for name, ch := range map[string]<-chan watchSSEEvent{"no param": plain, "other user": otherInbox, "pat": pat} {
		if got := chatEventsWithin(ch, 100*time.Millisecond); len(got) != 0 {
			t.Fatalf("%s stream received a notice: %+v", name, got)
		}
	}

	// A delete is a ref with deleted:true.
	f.must(f.request("DELETE", p+"/messages/2", "", nil), 200, "delete")
	got = chatEventsWithin(inbox, window)
	if len(got) != 1 || !got[0].Deleted || got[0].MessageSeq != 2 {
		t.Fatalf("inbox stream, delete: %+v", got)
	}
}
