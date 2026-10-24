package server

import (
	"fmt"
	"strings"
	"testing"
)

// TASK-4054: one owner, two agents. Agent alpha must not reach the owner's DM
// with agent bravo through any conversation endpoint, and must not leave a
// trace there: no bucket, no message, no marker move, no tombstone.
//
// Every {n} endpoint has two membership layers: resolveChatConversation and
// the endpoint's own store call with the full actor. A leak through the first
// layer alone (resolveChatConversation ignoring the agent key) is caught for
// send by its bucket, which is allocated before the store refuses, and for the
// other endpoints by the store layer still answering 404. Each {n} case asserts its
// endpoint on its own terms (status, body, side effects), so a leak through
// BOTH layers of one endpoint fails that endpoint's case. "No bucket" below
// means no bucket ALLOCATED; token spend on an existing bucket is not claimed.
//
// TASK-4060 adds the endpoints that could REVEAL bravo's DM without naming it
// (the inbox and the unread summary), and ensures that name bravo and an
// agent with no DM yet.
func TestChat_AgentCannotReachSiblingAgentDM(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "")
	f := newChatFixture(t)
	// TASK-4060 r1: seed both DMs through the store, not the API, so the
	// owner's ChatDM bucket is still unallocated when the ensure case runs and
	// its "no DM bucket" assertion can actually move.
	if _, _, err := f.srv.store.EnsureAgentDM(f.owner.ID, "alpha"); err != nil {
		t.Fatal(err)
	}
	bravoDM, _, err := f.srv.store.EnsureAgentDM(f.owner.ID, "bravo")
	if err != nil {
		t.Fatal(err)
	}
	bravo := fmt.Sprintf("%s/%d", chatBase, bravoDM.Number)
	bravoMsg := f.request("POST", bravo+"/messages", "bravo", map[string]any{"body": "from bravo"})
	f.must(bravoMsg, 201, "bravo send")
	bravoSeq := int64(chatMessage(t, bravoMsg)["seq"].(float64))
	// TASK-4060: unread rows on both sides. The owner has not read bravo's
	// message, so a leak that resolves alpha to the owner's member row (the
	// UserID-only actor) shows bravo's DM with unread 1. Bravo has not read
	// the owner's message, so a leak onto bravo's row would show it too.
	f.must(f.request("POST", bravo+"/messages", "", map[string]any{"body": "from owner"}), 201, "owner send")

	got := f.request("GET", bravo, "", nil)
	f.must(got, 200, "owner reads bravo DM")
	convID := chatObject(t, got)["id"].(string)
	missing := chatBase + "/999"

	cases := []struct {
		name, method, suffix string
		body                 any
	}{
		{"get", "GET", "", nil},
		{"list", "GET", "/messages", nil},
		{"send", "POST", "/messages", map[string]any{"body": "alpha intrudes"}},
		// TASK-4060: bravo's own message, so a leak that resolves alpha as bravo
		// would delete it (a 200 and a changed tombstone) rather than be refused.
		{"delete", "DELETE", fmt.Sprintf("/messages/%d", bravoSeq), nil},
		{"read", "POST", "/read", map[string]any{"seq": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Baselines per case, so a leak is reported by the case that caused it.
			before := chatDMState(t, f, convID)
			sendBuckets := chatBucketCount(f.srv.rateLimiters.ChatSend)
			rr := f.request(tc.method, bravo+tc.suffix, "alpha", tc.body)
			if rr.Code != 404 {
				t.Fatalf("alpha %s on bravo's DM: status %d, body %s", tc.name, rr.Code, rr.Body.String())
			}
			if after := chatDMState(t, f, convID); after != before {
				t.Fatalf("alpha %s changed bravo's DM: %s -> %s", tc.name, before, after)
			}
			if n := chatBucketCount(f.srv.rateLimiters.ChatSend); n != sendBuckets {
				t.Fatalf("alpha %s allocated a send bucket: %d -> %d", tc.name, sendBuckets, n)
			}
			ref := f.request(tc.method, missing+tc.suffix, "alpha", tc.body)
			if ref.Code != 404 {
				t.Fatalf("alpha %s on a missing conversation: status %d, body %s", tc.name, ref.Code, ref.Body.String())
			}
			if rr.Body.String() != ref.Body.String() {
				t.Fatalf("alpha %s: existence oracle: %q vs missing %q", tc.name, rr.Body.String(), ref.Body.String())
			}
		})
	}

	// TASK-4060 item 1a: alpha's inbox lists only alpha's DM, and its
	// unread_total excludes bravo's unread rows.
	t.Run("inbox", func(t *testing.T) {
		rr := f.request("GET", chatBase, "alpha", nil)
		f.must(rr, 200, "alpha inbox")
		out := chatObject(t, rr)
		for _, row := range out["conversations"].([]any) {
			c := row.(map[string]any)
			if c["id"] == convID || c["agent_key"] == "bravo" {
				t.Fatalf("alpha's inbox shows bravo's DM: %s", rr.Body.String())
			}
		}
		if out["unread_total"] != float64(0) {
			t.Fatalf("alpha's unread_total counts another DM: %s", rr.Body.String())
		}
	})

	// TASK-4060 item 1b: the unread summary carries no row for bravo's DM and
	// its total excludes it.
	t.Run("unread", func(t *testing.T) {
		rr := f.request("GET", "/api/v1/me/chat/unread", "alpha", nil)
		f.must(rr, 200, "alpha unread")
		out := chatObject(t, rr)
		for _, row := range out["conversations"].([]any) {
			c := row.(map[string]any)
			if c["conversation_id"] == convID || c["agent_key"] == "bravo" {
				t.Fatalf("alpha's unread summary shows bravo's DM: %s", rr.Body.String())
			}
		}
		if out["total"] != float64(0) {
			t.Fatalf("alpha's unread total counts another DM: %s", rr.Body.String())
		}
	})

	// TASK-4060 items 2 and 4: alpha cannot ensure (and so resolve or create)
	// another agent's DM by naming it. This is the one path that reaches
	// allowChatDM. The DM bucket is unallocated here (the DMs were seeded through
	// the store), so a charge before the refusal shows. Naming a key with no DM
	// yet ("charlie") is what lets a create before the refusal show as a row.
	t.Run("ensure", func(t *testing.T) {
		for _, key := range []string{"bravo", "charlie"} {
			dmBuckets := chatBucketCount(f.srv.rateLimiters.ChatDM)
			owner := f.request("GET", chatBase, "", nil)
			f.must(owner, 200, "owner inbox before")
			ownerRows := len(chatObject(t, owner)["conversations"].([]any))

			rr := f.request("POST", chatBase, "alpha", map[string]any{"agent_key": key})
			if rr.Code != 400 || !strings.Contains(rr.Body.String(), "agent_key must match X-Pad-Agent") {
				t.Fatalf("alpha ensuring %s's DM: status %d, body %s", key, rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), convID) {
				t.Fatalf("alpha's refused ensure of %s names bravo's DM: %s", key, rr.Body.String())
			}
			if n := chatBucketCount(f.srv.rateLimiters.ChatDM); n != dmBuckets {
				t.Fatalf("alpha's refused ensure of %s allocated a DM bucket: %d -> %d", key, dmBuckets, n)
			}
			owner = f.request("GET", chatBase, "", nil)
			f.must(owner, 200, "owner inbox after")
			if n := len(chatObject(t, owner)["conversations"].([]any)); n != ownerRows {
				t.Fatalf("alpha's refused ensure of %s created a conversation: %d -> %d", key, ownerRows, n)
			}
		}
	})
}

// chatDMState is the conversation's observable state: last_seq, every member's
// read and delivered markers, and every message's seq and deleted flag.
func chatDMState(t *testing.T, f *chatFixture, convID string) string {
	t.Helper()
	var b strings.Builder
	var lastSeq int64
	if err := f.srv.store.DB().QueryRow(`SELECT last_seq FROM conversations WHERE id = ?`, convID).Scan(&lastSeq); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "last_seq=%d", lastSeq)
	rows, err := f.srv.store.DB().Query(`SELECT agent_key, read_seq, delivered_seq FROM conversation_members WHERE conversation_id = ? ORDER BY agent_key`, convID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		var read, delivered int64
		if err := rows.Scan(&key, &read, &delivered); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, " member[%s]=%d/%d", key, read, delivered)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err = f.srv.store.DB().Query(`SELECT seq, deleted_at IS NOT NULL FROM conversation_messages WHERE conversation_id = ? ORDER BY seq`, convID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var deleted bool
		if err := rows.Scan(&seq, &deleted); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, " msg[%d]deleted=%t", seq, deleted)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func chatBucketCount(rl *ipRateLimiter) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.limiters)
}
