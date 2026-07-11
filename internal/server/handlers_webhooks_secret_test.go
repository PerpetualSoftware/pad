package server

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWebhookSecret_MaskedExceptOnCreate is the BUG-2057 API-surface regression:
// the raw HMAC secret is returned ONLY in the creation response and is masked
// (absent) from list responses, which instead expose only has_secret.
func TestWebhookSecret_MaskedExceptOnCreate(t *testing.T) {
	srv := testServer(t)

	// Configure an encryption key so the secret is genuinely encrypted at rest.
	key := make([]byte, 32)
	rand.Read(key)
	srv.store.SetEncryptionKey(key)

	token := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	owner, err := srv.store.GetUserByEmail("owner@test.com")
	if err != nil || owner == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Hooks", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	const secret = "top-secret-hmac-key"
	base := "/api/v1/workspaces/" + ws.Slug + "/webhooks"

	// CREATE — the raw secret MUST be echoed back exactly once.
	rr := doRequestWithCookie(srv, "POST", base, map[string]any{
		"url":    "https://example.com/hook",
		"secret": secret,
	}, token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var created models.Webhook
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Secret != secret {
		t.Errorf("create response should return raw secret %q, got %q", secret, created.Secret)
	}
	if !created.HasSecret {
		t.Error("create response should report has_secret=true")
	}

	// LIST — the raw secret must NOT appear anywhere in the response body.
	rr = doRequestWithCookie(srv, "GET", base, nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if body := rr.Body.String(); strings.Contains(body, secret) {
		t.Fatalf("raw secret leaked in list response: %s", body)
	}
	var listed []models.Webhook
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("expected 1 webhook, got %d", len(listed))
	}
	if listed[0].Secret != "" {
		t.Errorf("list response should mask secret, got %q", listed[0].Secret)
	}
	if !listed[0].HasSecret {
		t.Error("list response should still report has_secret=true")
	}
}

// TestWebhookSecret_RejectsReservedPrefix pins the review follow-up: a create
// request whose secret starts with the reserved "enc:" marker is rejected, so a
// user plaintext can never masquerade as ciphertext at rest.
func TestWebhookSecret_RejectsReservedPrefix(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	owner, err := srv.store.GetUserByEmail("owner@test.com")
	if err != nil || owner == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Hooks", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/webhooks", map[string]any{
		"url":    "https://example.com/hook",
		"secret": "enc:sneaky",
	}, token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for reserved-prefix secret, got status=%d body=%s", rr.Code, rr.Body.String())
	}
}
