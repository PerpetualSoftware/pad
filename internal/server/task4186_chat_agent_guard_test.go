package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/agentchatcases"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4186 PR C: the client guard (cli.CheckAgentChatTarget) is only a
// pre-check, so it must refuse exactly what the real ensure handler refuses.
// The cli test runs the same table through the guard; this half drives the
// real handler. A row the guard refuses and the server accepts is a false
// refusal; the reverse is a guard that stopped guarding.
func TestChat_AgentChatTargetGuardParity(t *testing.T) {
	f := newChatFixture(t)
	for _, tc := range agentchatcases.Cases() {
		if tc.Key == "" {
			continue // C<n> / <n> never reaches the ensure handler
		}
		t.Run(tc.Name, func(t *testing.T) {
			rr := f.request("POST", chatBase, tc.Caller, map[string]any{"agent_key": tc.Key})
			refused := rr.Code == 400
			if !refused && rr.Code != 200 && rr.Code != 201 {
				t.Fatalf("unexpected status %d: %s", rr.Code, rr.Body.String())
			}
			if refused != tc.Refuse {
				t.Fatalf("server ensure(caller %q, key %q) status %d, want refuse=%v", tc.Caller, tc.Key, rr.Code, tc.Refuse)
			}
		})
	}
}

func TestNormalizeAgentNameDelegatesToModels(t *testing.T) {
	for _, tc := range agentchatcases.Cases() {
		for _, in := range []string{tc.Caller, tc.Key} {
			if got, want := normalizeAgentName(in), models.NormalizeAgentName(in); got != want {
				t.Fatalf("normalizeAgentName(%q) = %q, models = %q", in, got, want)
			}
		}
	}
}
