package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/agentchatcases"
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
	stub := &chatStub{}
	ts := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer ts.Close()
	c := NewClientFromURLWithAgentName(ts.URL, "director")
	_, err := c.ResolveChatTarget("@dave", true)
	if err == nil {
		t.Fatal("send: want refusal")
	}
	if len(stub.reqs) != 0 {
		t.Fatalf("refusal still sent %v", stub.reqs)
	}
	for _, other := range []string{"secret-sibling", "hidden-bot"} {
		if strings.Contains(err.Error(), other) {
			t.Fatalf("error names another agent %q: %v", other, err)
		}
	}
	if !strings.Contains(err.Error(), `@director`) || !strings.Contains(err.Error(), "C<n>") {
		t.Fatalf("error does not point at the caller's own target: %v", err)
	}
}

// show, read and delete resolve with ensure=false: an agent's @other keeps the
// truthful not_found from the lookup, and the guard (whose error is not an
// APIError) is not invoked.
func TestResolveChatTargetNoEnsureKeepsNotFound(t *testing.T) {
	stub := &chatStub{}
	ts := httptest.NewServer(http.HandlerFunc(stub.handler))
	defer ts.Close()
	c := NewClientFromURLWithAgentName(ts.URL, "director")
	_, err := c.ResolveChatTarget("@dave", false)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
		t.Fatalf("want not_found APIError, got %v", err)
	}
	if strings.Contains(err.Error(), "one chat") {
		t.Fatalf("guard ran on a no-ensure resolve: %v", err)
	}
	if len(stub.reqs) != 1 || stub.reqs[0] != "GET /api/v1/me/conversations" {
		t.Fatalf("want exactly the lookup, got %v", stub.reqs)
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
