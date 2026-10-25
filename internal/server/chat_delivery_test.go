package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

func chatNotice() watchevents.Notification {
	return watchevents.Notification{Kind: watchevents.KindChat, ConversationID: "conv", ConversationNumber: 1, MessageID: "msg", MessageSeq: 1, TargetUserID: "u", TargetAgentKey: "rook"}
}
func TestChatKindRequiresOptInArmedAgentMatch(t *testing.T) {
	for _, change := range []string{"none", "chat", "armed", "user", "agent", "empty-target-agent", "empty-agent", "both-empty-agents", "empty-user"} {
		t.Run(change, func(t *testing.T) {
			n := chatNotice()
			p := watchStreamPrefs{chat: true, armed: true, agentKey: "rook"}
			u := "u"
			switch change {
			case "chat":
				p.chat = false
			case "armed":
				p.armed = false
			case "user":
				u = "other"
			case "agent":
				p.agentKey = "owl"
			case "empty-target-agent":
				n.TargetAgentKey = ""
			case "empty-agent":
				p.agentKey = ""
			case "both-empty-agents":
				n.TargetAgentKey = ""
				p.agentKey = ""
			case "empty-user":
				n.TargetUserID = ""
				u = ""
			}
			if got := watchStreamDelivers(map[string]string{"": ""}, watchAccessVisibility{}, u, "", p, n); got != (change == "none") {
				t.Fatalf("delivery=%v", got)
			}
		})
	}
	f := newChatFixture(t)
	h := httptest.NewServer(f.srv)
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, f.ownerTok, map[string]string{"X-Pad-Agent": "rook"}, "armed=true&chat=true")
	waitForWatchEvent(t, ch, time.Second)
	p := f.ensure("rook")
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "hello"}), 201, "send")
	ev := waitForWatchEvent(t, ch, time.Second)
	if !strings.Contains(ev.Data, `"kind":"chat"`) {
		t.Fatal(ev)
	}
}
func TestChatKindNotWatchMatched(t *testing.T) {
	n := chatNotice()
	n.ItemID = "watched"
	watches := map[string]string{"watched": ""}
	vis := watchAccessVisibility{fullAccess: true}
	if watchNotificationVisible(watches, vis, "u", "", true, n) {
		t.Fatal("chat leaked through watch")
	}
	for _, p := range []watchStreamPrefs{{armed: true, chat: true, agentKey: "owl"}, {armed: true, chat: true, agentKey: "rook"}} {
		if watchStreamDelivers(watches, vis, "other", "", p, n) {
			t.Fatal("watch bypassed addressing")
		}
	}
}
func TestChatPublishRefOnlyNoBody(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"rune-bound", strings.Repeat("界", 120) + "SECRET-BODY-xyz"},
		{"first-line", "public preview\nSECOND-LINE-SECRET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			f := newChatFixture(t)
			p := f.ensure("rook")
			mark := f.mark()
			f.must(f.request("POST", p+"/messages", "", map[string]any{"body": body}), 201, "send")
			ns := f.srv.watchEvents.EventsSince(mark)
			// TASK-4044: the agent notice plus the human author's self-notice.
			if len(ns) != 2 {
				t.Fatalf("notices=%v", ns)
			}
			n := chatNoticeFor(t, ns, "rook")
			b, _ := json.Marshal(n)
			wire, _ := json.Marshal(watchEventPayloadFor(nil, n))
			if strings.Contains(string(b)+string(wire), "SECRET-BODY-xyz") || strings.Contains(string(b)+string(wire), "SECOND-LINE-SECRET") || len([]rune(n.Excerpt)) > 100 || n.Excerpt != models.QuoteExcerpt(body) || n.Summary != "" {
				t.Fatalf("body leak: %s %s", b, wire)
			}
			if n.ConversationID == "" || n.ConversationNumber != 1 || n.MessageID == "" || n.MessageSeq != 1 || n.AuthorUserID != f.owner.ID || n.AuthorAgentKey != "" || n.TargetUserID != f.owner.ID || n.TargetAgentKey != "rook" {
				t.Fatalf("bad ref: %+v", n)
			}
		})
	}
}
func TestChatPublishSkipsAuthorAndHuman(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	mark := f.mark()
	f.must(f.request("POST", p+"/messages", "rook", map[string]any{"body": "agent"}), 201, "agent send")
	// TASK-4044: an agent's send reaches its owner's inbox, not the author.
	if ns := f.srv.watchEvents.EventsSince(mark); len(ns) != 1 || ns[0].TargetAgentKey != "" || ns[0].TargetUserID != f.owner.ID {
		t.Fatal(ns)
	}
	mark = f.mark()
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "human"}), 201, "human send")
	ns := f.srv.watchEvents.EventsSince(mark)
	if len(ns) != 2 || chatNoticeFor(t, ns, "rook").TargetAgentKey != "rook" {
		t.Fatal(ns)
	}
	// TASK-4041: defend the author pair even if a caller includes it.
	c, _, _ := f.srv.store.EnsureAgentDM(f.owner.ID, "rook")
	mark = f.mark()
	f.srv.notifyChatMessage(c, models.ConversationMessage{AuthorUserID: f.owner.ID, AuthorAgentKey: "rook"}, []models.ConversationActor{{UserID: f.owner.ID, AgentKey: "rook"}, {UserID: f.owner.ID}}, false)
	// The author pair gets nothing; the human member gets exactly one inbox
	// notice (TASK-4044), deduplicated against the author's own user.
	if ns := f.srv.watchEvents.EventsSince(mark); len(ns) != 1 || ns[0].TargetAgentKey != "" {
		t.Fatalf("author notified or duplicated: %+v", ns)
	}
}
func TestChatReplayDoesNotPublish(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	in := map[string]any{"body": "hi", "op_key": "once"}
	f.must(f.request("POST", p+"/messages", "", in), 201, "send")
	mark := f.mark()
	f.must(f.request("POST", p+"/messages", "", in), 200, "replay")
	if f.mark() != mark {
		t.Fatal("replay published")
	}
	checkChatReplayPublicationGuard(t)
}
func TestChatDeletePublishesRetractionOnce(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "hi"}), 201, "send")
	mark := f.mark()
	f.must(f.request("DELETE", p+"/messages/1", "rook", nil), 403, "forbidden")
	if f.mark() != mark {
		t.Fatal("forbidden published")
	}
	f.must(f.request("DELETE", p+"/messages/1", "", nil), 200, "delete")
	ns := f.srv.watchEvents.EventsSince(mark)
	if len(ns) != 2 || !ns[0].ChatDeleted || ns[0].Excerpt != "" || ns[0].MessageID == "" {
		t.Fatal(ns)
	}
	mark = f.mark()
	f.must(f.request("DELETE", p+"/messages/1", "", nil), 200, "again")
	if f.mark() != mark {
		t.Fatal("repeat published")
	}
}
func TestChatSendNeverAdvancesMarkers(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	for _, a := range []string{"", "rook"} {
		f.must(f.request("POST", p+"/messages", a, map[string]any{"body": "hi"}), 201, "send")
	}
	rows, err := f.srv.store.DB().Query(`SELECT read_seq, delivered_seq FROM conversation_members`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r, d int
		if err := rows.Scan(&r, &d); err != nil {
			t.Fatal(err)
		}
		if r != 0 || d != 0 {
			t.Fatalf("markers %d %d", r, d)
		}
	}
}
func TestChatDeliveredEndpoint(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	for i := 0; i < 3; i++ {
		f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "hi"}), 201, "send")
	}
	for i, seq := range []int{2, 1, 999} {
		rr := f.request("POST", p+"/delivered", "rook", map[string]any{"seq": seq})
		f.must(rr, 200, "ack")
		want := []float64{2, 2, 3}[i]
		if chatObject(t, rr)["delivered_seq"] != want {
			t.Fatal(rr.Body.String())
		}
	}
	for _, in := range []any{map[string]any{}, map[string]any{"seq": -1}, map[string]any{"seq": 1.5}} {
		f.must(f.request("POST", p+"/delivered", "rook", in), 400, "invalid")
	}
	a := f.request("POST", p+"/delivered", "owl", map[string]any{"seq": 1})
	b := f.request("POST", chatBase+"/999/delivered", "owl", map[string]any{"seq": 1})
	f.must(a, 404, "wrong agent")
	if a.Body.String() != b.Body.String() {
		t.Fatal("oracle")
	}
	var human int
	err := f.srv.store.DB().QueryRow(`SELECT delivered_seq FROM conversation_members WHERE agent_key=''`).Scan(&human)
	if err != nil || human != 0 {
		t.Fatalf("wrong actor: %d %v", human, err)
	}
}
func TestChatDirectionNotSettableFromServer(t *testing.T) {
	re := regexp.MustCompile(`(?i)direction|framing|accept|instruction`)
	for _, v := range []any{watchevents.Notification{}, watchEventPayload{}, chatMsg{}} {
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if re.MatchString(f.Name + string(f.Tag)) {
				t.Fatal(f)
			}
		}
	}
	// Check anonymous handler request structs too.
	checkChatRequestFields(t, re)
	f := newChatFixture(t)
	h := httptest.NewServer(f.srv)
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	plain := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, f.ownerTok, map[string]string{"X-Pad-Agent": "rook"}, "chat=true&armed=true")
	shaped := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, f.ownerTok, map[string]string{"X-Pad-Agent": "rook", "X-Pad-Direction": "true"}, "chat=true&armed=true&direction=true&framing=direction&accept_instructions=true&instruction=true")
	waitForWatchEvent(t, plain, time.Second)
	waitForWatchEvent(t, shaped, time.Second)
	p := f.ensure("rook")
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "hi"}), 201, "send")
	payloads := make([]map[string]any, 2)
	for i, ch := range []<-chan watchSSEEvent{plain, shaped} {
		e := waitForWatchEvent(t, ch, time.Second)
		if err := json.Unmarshal([]byte(e.Data), &payloads[i]); err != nil {
			t.Fatal(err)
		}
		if payloads[i]["kind"] != "chat" {
			t.Fatalf("expected delivered chat: %+v", e)
		}
		for key, value := range payloads[i] {
			text, _ := value.(string)
			if re.MatchString(key) || re.MatchString(text) {
				t.Fatalf("direction-shaped payload: %s", e.Data)
			}
		}
	}
	// The same event must arrive unchanged, including the exact key set.
	if !reflect.DeepEqual(payloads[0], payloads[1]) {
		t.Fatalf("query/header changed chat payload: plain=%v shaped=%v", payloads[0], payloads[1])
	}
}
func TestWatchStreamChatPayloadShape(t *testing.T) {
	f := newChatFixture(t)
	h := httptest.NewServer(f.srv)
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, f.ownerTok, map[string]string{"X-Pad-Agent": "rook"}, "armed=true")
	waitForWatchEvent(t, ch, time.Second)
	path := f.ensure("rook")
	f.must(f.request("POST", path+"/messages", "", map[string]any{"body": "hidden"}), 201, "send")
	select {
	case e := <-ch:
		t.Fatalf("no opt-in leaked %+v", e)
	case <-time.After(60 * time.Millisecond):
	}
	n := chatNotice()
	n.ChatDeleted = true
	p := watchEventPayloadFor(nil, n)
	b, _ := json.Marshal(p)
	for _, key := range []string{`"conversation_id":"conv"`, `"conversation":1`, `"message_id":"msg"`, `"message_seq":1`, `"deleted":true`} {
		if !strings.Contains(string(b), key) {
			t.Fatal(string(b))
		}
	}
	legacy, _ := json.Marshal(watchEventPayload{ID: 1, Kind: "comment"})
	if string(legacy) != `{"id":1,"ts":0,"workspace":"","item_ref":"","kind":"comment","actor":"","summary":""}` {
		t.Fatal(string(legacy))
	}
	if watchStreamDelivers(nil, watchAccessVisibility{}, "u", "", watchStreamPrefs{armed: true, agentKey: "rook"}, n) {
		t.Fatal("missing opt in")
	}
}
func TestChatStreamDeniedWhenUserDisabled(t *testing.T) {
	f := newChatFixture(t)
	r := httptest.NewRequest("GET", "/", nil)
	cache := newWatchVisCache(f.srv, r, f.owner)
	if err := f.srv.store.DisableUserAndRevokeAccess(f.owner.ID); err != nil {
		t.Fatal(err)
	}
	cache.reset()
	if !cache.denied() {
		t.Fatal("disabled user not denied")
	}
	if cache.delivers(nil, "u", "", watchStreamPrefs{armed: true, chat: true, agentKey: "rook"}, chatNotice()) {
		t.Fatal("disabled delivered")
	}
}

// TASK-4041: chat-only streams have no workspace visibility lookup to refresh
// identity incidentally. Their reval tick must reset denial even on reload errors.
func TestChatStreamTickDeniesAndRecovers(t *testing.T) {
	f := newChatFixture(t)
	tick := make(chan time.Time)
	f.srv.watchRevalTickOverride.Store(&tick)
	h := httptest.NewServer(f.srv)
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := connectWatchStreamWithHeadersAndQuery(ctx, t, h.URL, f.ownerTok, map[string]string{"X-Pad-Agent": "rook"}, "chat=true&armed=true")
	waitForWatchEvent(t, ch, time.Second)
	n := chatNotice()
	n.TargetUserID = f.owner.ID
	publish := func() { f.srv.publishWatchNotification(n) }
	publish()
	if e := waitForWatchEvent(t, ch, time.Second); !strings.Contains(e.Data, `"kind":"chat"`) {
		t.Fatal(e)
	}
	processed := make(chan struct{}, 1)
	fault := func() error { processed <- struct{}{}; return errFakeWatchListReload }
	f.srv.watchPredicatesLoadFault.Store(&fault)
	driveTick := func() {
		t.Helper()
		select {
		case tick <- time.Time{}:
		case <-time.After(3 * time.Second):
			t.Fatal("tick not received")
		}
		select {
		case <-processed:
		case <-time.After(3 * time.Second):
			t.Fatal("tick not processed; stream closed or watch reload skipped")
		}
	}
	// Preserve the credential so closure on revocation cannot mask denial.
	if err := f.srv.store.DisableUserLeavingCredentialsForTesting(f.owner.ID); err != nil {
		t.Fatal(err)
	}
	driveTick()
	publish()
	select {
	case e := <-ch:
		t.Fatalf("disabled chat-only stream delivered or closed: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
	if err := f.srv.store.EnableUser(f.owner.ID); err != nil {
		t.Fatal(err)
	}
	driveTick()
	n.MessageID = "after-enable"
	publish()
	if e := waitForWatchEvent(t, ch, time.Second); !strings.Contains(e.Data, `"message_id":"after-enable"`) {
		t.Fatal(e)
	}
}

// chatNoticeFor returns the one notice targeting agentKey ("" for the human
// inbox notice) and fails if there is not exactly one.
func chatNoticeFor(t *testing.T, ns []watchevents.Notification, agentKey string) watchevents.Notification {
	t.Helper()
	var out []watchevents.Notification
	for _, n := range ns {
		if n.TargetAgentKey == agentKey {
			out = append(out, n)
		}
	}
	if len(out) != 1 {
		t.Fatalf("want one notice for agent %q, got %+v", agentKey, ns)
	}
	return out[0]
}
