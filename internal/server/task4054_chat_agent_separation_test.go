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
// the endpoint's own store call with the full actor. The director's mutant
// (TASK-4039 GO audit) dropped the agent key from the first layer; the second
// layer still answered 404, so only the send bucket betrayed it. Each case
// below asserts its endpoint on its own terms (status, body, side effects),
// so a leak in any one endpoint fails that case, not a shared side effect.
func TestChat_AgentCannotReachSiblingAgentDM(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "")
	f := newChatFixture(t)
	f.ensure("alpha")
	bravo := f.ensure("bravo")
	f.must(f.request("POST", bravo+"/messages", "bravo", map[string]any{"body": "from bravo"}), 201, "bravo send")
	human := f.request("POST", bravo+"/messages", "", map[string]any{"body": "from owner"})
	f.must(human, 201, "owner send")
	humanSeq := int64(chatMessage(t, human)["seq"].(float64))

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
		{"delete", "DELETE", fmt.Sprintf("/messages/%d", humanSeq), nil},
		{"read", "POST", "/read", map[string]any{"seq": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Baselines per case, so a leak is reported by the case that caused it.
			before := chatDMState(t, f, convID)
			sendBuckets, dmBuckets := chatBucketCount(f.srv.rateLimiters.ChatSend), chatBucketCount(f.srv.rateLimiters.ChatDM)
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
			if n := chatBucketCount(f.srv.rateLimiters.ChatDM); n != dmBuckets {
				t.Fatalf("alpha %s allocated a DM bucket: %d -> %d", tc.name, dmBuckets, n)
			}
			ref := f.request(tc.method, missing+tc.suffix, "alpha", tc.body)
			if rr.Body.String() != ref.Body.String() {
				t.Fatalf("alpha %s: existence oracle: %q vs missing %q", tc.name, rr.Body.String(), ref.Body.String())
			}
		})
	}
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
	return b.String()
}

func chatBucketCount(rl *ipRateLimiter) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.limiters)
}
