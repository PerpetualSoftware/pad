package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/decision"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// Tests for POST /workspaces/{ws}/playbooks/match (PLAN-3114 unit 5,
// TASK-3120): a typed-decision Choice over the workspace's active
// playbooks, plus a reserved "none" option.

// fakeMatchProvider is a decision.Provider whose Ask is fully caller-driven
// via askFn, and which records every call for assertions on what the
// handler actually SENT — not just what it returned.
type fakeMatchProvider struct {
	mu    sync.Mutex
	calls int
	askFn func(state any, qs map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error)
}

func (p *fakeMatchProvider) Name() string  { return "fake" }
func (p *fakeMatchProvider) Model() string { return "fake-model-1" }
func (p *fakeMatchProvider) Ask(_ context.Context, state any, qs map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.askFn == nil {
		return nil, decision.Usage{}, errors.New("fakeMatchProvider: no askFn configured")
	}
	return p.askFn(state, qs)
}

func (p *fakeMatchProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func attachMatchRunner(srv *Server, p decision.Provider) {
	srv.SetDecisionRunner(decision.NewRunner(srv.store, p, nil))
}

func ptrFloat(f float64) *float64 { return &f }

func matchRequestBody(text string) map[string]any {
	return map[string]any{"text": text}
}

// TestPlaybookMatch_NoProviderIs404 — the fall-back-to-slug/trigger-routing
// case: no typed-decision provider configured at all.
func TestPlaybookMatch_NoProviderIs404(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/match", matchRequestBody("ship these tasks"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	parseJSON(t, rr, &body)
	if body.Error.Code != "decision_provider_unavailable" {
		t.Errorf("code = %q, want decision_provider_unavailable", body.Error.Code)
	}
}

// TestPlaybookMatch_ZeroActivePlaybooksNoProviderCall covers the "workspace
// has nothing to match against" case: a legitimate 200 answer, not an
// error, and — since a Choice needs at least two options and "none" alone
// isn't a meaningful question — no provider call is made at all.
func TestPlaybookMatch_ZeroActivePlaybooksNoProviderCall(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	p := &fakeMatchProvider{}
	attachMatchRunner(srv, p)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/match", matchRequestBody("ship these tasks"))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp PlaybookMatchResponse
	parseJSON(t, rr, &resp)
	if resp.Choice != "" || resp.Reason != "no_active_playbooks" {
		t.Errorf("resp = %+v, want empty choice + reason=no_active_playbooks", resp)
	}
	if resp.Options == nil || len(resp.Options) != 0 {
		t.Errorf("options = %v, want an empty (non-null) slice", resp.Options)
	}
	if p.callCount() != 0 {
		t.Errorf("provider called %d times, want 0 (nothing to ask about)", p.callCount())
	}
}

// TestPlaybookMatch_DraftAndDeprecatedExcluded_ExactPrompt pins BOTH the
// filtering (draft/deprecated playbooks never reach the provider) and the
// exact prompt text sent — so a future edit to the wording or the option
// description shows up as a visible diff here rather than silently
// drifting (lead's amendment 7).
func TestPlaybookMatch_DraftAndDeprecatedExcluded_ExactPrompt(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	active := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Ship a release",
		"content": "Cut a release and publish it.\n\nMore detail below.",
		"fields":  `{"status":"active","trigger":"on-release","invocation_slug":"ship"}`,
	})
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Draft playbook",
		"fields": `{"status":"draft"}`,
	})
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Retired playbook",
		"fields": `{"status":"deprecated"}`,
	})

	var captured map[string]decision.Question
	p := &fakeMatchProvider{askFn: func(_ any, qs map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
		captured = qs
		return map[string]decision.Answer{
			"match": {
				Kind:          decision.KindChoice,
				Choice:        active.Ref,
				Confidence:    ptrFloat(0.9),
				Probabilities: map[string]float64{active.Ref: 0.9, "none": 0.1},
			},
		}, decision.Usage{}, nil
	}}
	attachMatchRunner(srv, p)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/match", matchRequestBody("cut a release please"))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	q, ok := captured["match"]
	if !ok {
		t.Fatal("provider did not receive a 'match' question")
	}
	wantInstructions := `Given the text, does it ask for one of these procedures to be run? Pick the option that best matches what the text is asking for, or "none" if it does not ask for any of them.`
	if q.Instructions != wantInstructions {
		t.Errorf("instructions = %q, want %q", q.Instructions, wantInstructions)
	}
	if len(q.Options) != 2 {
		t.Fatalf("options sent = %v, want exactly 2 (the active playbook + none — draft/deprecated excluded)", q.Options)
	}
	wantDesc := `Ship a release: Cut a release and publish it. (trigger: on-release; invoke via slug "ship")`
	if q.Options[active.Ref] != wantDesc {
		t.Errorf("option description = %q, want %q", q.Options[active.Ref], wantDesc)
	}
	if q.Options["none"] != "the text does not ask for any of the procedures listed above" {
		t.Errorf(`"none" description = %q`, q.Options["none"])
	}

	var resp PlaybookMatchResponse
	parseJSON(t, rr, &resp)
	if resp.Choice != active.Ref {
		t.Errorf("choice = %q, want %q", resp.Choice, active.Ref)
	}
	if len(resp.Options) != 1 || resp.Options[0].Ref != active.Ref {
		t.Errorf("response options = %+v, want exactly the active playbook", resp.Options)
	}
	if resp.Options[0].InvocationSlug != "ship" {
		t.Errorf("invocation_slug = %q, want ship", resp.Options[0].InvocationSlug)
	}
}

// TestPlaybookMatch_OneActivePlaybookIsLegal covers the edge case the
// reserved "none" option exists to fix for free: decision.Question.Validate
// refuses a Choice with fewer than 2 options, so a workspace with exactly
// one active playbook would otherwise have no legal question to ask.
func TestPlaybookMatch_OneActivePlaybookIsLegal(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	only := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Only playbook",
		"fields": `{"status":"active"}`,
	})

	var optionCount int
	p := &fakeMatchProvider{askFn: func(_ any, qs map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
		optionCount = len(qs["match"].Options)
		return map[string]decision.Answer{"match": {Kind: decision.KindChoice, Choice: "none", Confidence: ptrFloat(0.6)}}, decision.Usage{}, nil
	}}
	attachMatchRunner(srv, p)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/match", matchRequestBody("unrelated text"))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if optionCount != 2 {
		t.Errorf("options sent = %d, want 2 (the one playbook + none)", optionCount)
	}
	var resp PlaybookMatchResponse
	parseJSON(t, rr, &resp)
	if len(resp.Options) != 1 || resp.Options[0].Ref != only.Ref {
		t.Errorf("response options = %+v, want the one playbook", resp.Options)
	}
}

// TestPlaybookMatch_ProviderErrorIs502 — a provider outage must be
// distinguishable from "no provider configured" (404): a caller reading a
// 502 knows the provider IS configured and try-again may help; a caller
// reading 404 should switch to slug/trigger routing instead.
func TestPlaybookMatch_ProviderErrorIs502(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Ship it",
		"fields": `{"status":"active"}`,
	})
	p := &fakeMatchProvider{askFn: func(_ any, _ map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
		return nil, decision.Usage{}, errors.New("upstream exploded")
	}}
	attachMatchRunner(srv, p)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/match", matchRequestBody("ship these tasks"))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	parseJSON(t, rr, &body)
	if body.Error.Code != "decision_provider_error" {
		t.Errorf("code = %q, want decision_provider_error", body.Error.Code)
	}
}

// TestPlaybookMatch_OutOfSetChoiceIs502 — the provider naming an option it
// was never offered must never pass through to the caller (lead's amendment
// 2). Distinguished from a plain provider error only by cause, not by code:
// both are "the provider misbehaved", covered by the same
// decision_provider_error.
func TestPlaybookMatch_OutOfSetChoiceIs502(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Ship it",
		"fields": `{"status":"active"}`,
	})
	p := &fakeMatchProvider{askFn: func(_ any, _ map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
		return map[string]decision.Answer{
			"match": {Kind: decision.KindChoice, Choice: "PLAYB-9999-DOES-NOT-EXIST", Confidence: ptrFloat(0.5)},
		}, decision.Usage{}, nil
	}}
	attachMatchRunner(srv, p)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/match", matchRequestBody("ship these tasks"))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]json.RawMessage
	parseJSON(t, rr, &body)
	if _, hasChoice := body["choice"]; hasChoice {
		t.Errorf("response leaked a 'choice' key on the refused answer: %s", rr.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	parseJSON(t, rr, &errBody)
	if errBody.Error.Code != "decision_provider_error" {
		t.Errorf("code = %q, want decision_provider_error", errBody.Error.Code)
	}
}

// TestPlaybookMatch_TextBounds covers the empty/whitespace and length
// gates. The at-limit leg reaches the provider (no length refusal), the
// over-limit leg never does.
func TestPlaybookMatch_TextBounds(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Ship it",
		"fields": `{"status":"active"}`,
	})
	p := &fakeMatchProvider{askFn: func(_ any, _ map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
		return map[string]decision.Answer{"match": {Kind: decision.KindChoice, Choice: "none", Confidence: ptrFloat(0.5)}}, decision.Usage{}, nil
	}}
	attachMatchRunner(srv, p)

	cases := []struct {
		name       string
		text       string
		wantStatus int
	}{
		{"empty", "", http.StatusBadRequest},
		{"whitespace only", "   \t\n  ", http.StatusBadRequest},
		{"over limit", strings.Repeat("a", maxPlaybookMatchTextRunes+1), http.StatusBadRequest},
		{"at limit", strings.Repeat("a", maxPlaybookMatchTextRunes), http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/match", matchRequestBody(c.text))
			if rr.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rr.Code, c.wantStatus, rr.Body.String())
			}
		})
	}
}

// TestPlaybookMatch_InvisiblePlaybookExcludedFromOptions — a guest holding
// an item-level grant on only ONE of two active playbooks must never see
// the other one in Options, matching the visibility rule `pad playbook
// list` already enforces (listPlaybookItems is shared by both handlers).
//
// Driven directly against the handler with a synthesized guest context
// (mirrors handlers_attachments_test.go's uploadAsGuestRepeated) since the
// router's normal auth stack doesn't expose a way to authenticate as a
// grant-only guest in-process.
func TestPlaybookMatch_InvisiblePlaybookExcludedFromOptions(t *testing.T) {
	srv := testServer(t)
	// createWSWithCollections seeds the real "playbooks" system collection
	// (with its real bootstrap_include trait) via the startup template —
	// reused rather than hand-rolling a trait declaration, so this test
	// exercises the same collection shape every other playbook test does.
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%q): %v", slug, err)
	}

	granted := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Granted playbook",
		"fields": `{"status":"active"}`,
	})
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Ungranted playbook",
		"fields": `{"status":"active"}`,
	})

	guest, err := srv.store.CreateUser(models.UserCreate{
		Email: "guest@test.com", Name: "Guest", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, granted.ID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	p := &fakeMatchProvider{askFn: func(_ any, _ map[string]decision.Question) (map[string]decision.Answer, decision.Usage, error) {
		return map[string]decision.Answer{"match": {Kind: decision.KindChoice, Choice: "none", Confidence: ptrFloat(0.5)}}, decision.Usage{}, nil
	}}
	attachMatchRunner(srv, p)

	body, _ := json.Marshal(matchRequestBody("do the granted thing"))
	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+ws.ID+"/playbooks/match", bytes.NewReader(body))
	ctx := req.Context()
	ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, ws.ID)
	ctx = context.WithValue(ctx, ctxCurrentUser, guest)
	ctx = context.WithValue(ctx, ctxWorkspaceRole, "guest")
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	srv.handleMatchPlaybook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp PlaybookMatchResponse
	parseJSON(t, rr, &resp)
	if len(resp.Options) != 1 || resp.Options[0].Ref != granted.Ref {
		t.Fatalf("options = %+v, want exactly the granted playbook (%s)", resp.Options, granted.Ref)
	}
}
