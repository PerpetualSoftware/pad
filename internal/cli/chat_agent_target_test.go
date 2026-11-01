package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/agentchatcases"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestCheckAgentChatTargetSharedTable(t *testing.T) {
	for _, tc := range agentchatcases.Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			err := CheckAgentChatTarget(tc.Caller, tc.Target)
			if (err != nil) != tc.Refuse {
				t.Fatalf("CheckAgentChatTarget(%q, %q) = %v, want refuse=%v", tc.Caller, tc.Target, err, tc.Refuse)
			}
		})
	}
}

// chatStub records every request and answers conversations with other agents'
// DMs, so a leak of their keys into an error is visible.
type chatStub struct {
	mu   sync.Mutex
	reqs []string
}

func (s *chatStub) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		_, _ = w.Write([]byte(`{"conversation":{"number":7}}`))
		return
	}
	_, _ = w.Write([]byte(`{"conversations":[{"number":1,"agent_key":"secret-sibling"},{"number":2,"agent_key":"hidden-bot"}]}`))
}

func TestResolveChatTargetGuardSendsNothingOnRefusal(t *testing.T) {
	for _, ensure := range []bool{true, false} {
		stub := &chatStub{}
		ts := httptest.NewServer(http.HandlerFunc(stub.handler))
		c := NewClientFromURLWithAgentName(ts.URL, "director")
		_, err := c.ResolveChatTarget("@dave", ensure)
		ts.Close()
		if err == nil {
			t.Fatalf("ensure=%v: want refusal", ensure)
		}
		if len(stub.reqs) != 0 {
			t.Fatalf("ensure=%v: refusal still sent %v", ensure, stub.reqs)
		}
		for _, other := range []string{"secret-sibling", "hidden-bot", "dave"} {
			if other == "dave" {
				continue // the typed target is allowed in the message
			}
			if strings.Contains(err.Error(), other) {
				t.Fatalf("error names another agent %q: %v", other, err)
			}
		}
		if !strings.Contains(err.Error(), `@director`) || !strings.Contains(err.Error(), "C<n>") {
			t.Fatalf("error does not point at the caller's own target: %v", err)
		}
	}
}

func TestResolveChatTargetGuardPasses(t *testing.T) {
	stub := &chatStub{}
	ts := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer ts.Close()
	c := NewClientFromURLWithAgentName(ts.URL, "director ")
	n, err := c.ResolveChatTarget("@director", true)
	if err != nil || n != 7 {
		t.Fatalf("own DM: n=%d err=%v", n, err)
	}
	if len(stub.reqs) != 2 {
		t.Fatalf("requests = %v, want the list then the ensure", stub.reqs)
	}
	human := NewClientFromURLWithAgentName(ts.URL, "")
	if n, err := human.ResolveChatTarget("@rook", true); err != nil || n != 7 {
		t.Fatalf("human: n=%d err=%v", n, err)
	}
}

func TestNormalizeAgentNameCases(t *testing.T) {
	if got := models.NormalizeAgentName(" a\tb "); got != "ab" {
		t.Fatalf("got %q", got)
	}
}
