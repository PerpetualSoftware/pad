package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TASK-3866: both doors that mint an API key accept exactly the scope
// values tokenScopeAllows recognizes, store them compact, and refuse
// anything that would mint a key the server then refuses everywhere, or
// one in the legacy unrestricted [] form.
func TestTASK3866_CreateTokenScopes(t *testing.T) {
	t.Parallel()
	env := setupTokenAuthEnv(t)
	doors := map[string]string{
		"user":      "/api/v1/auth/tokens",
		"workspace": "/api/v1/workspaces/" + env.ws.Slug + "/tokens",
	}

	accepted := []struct {
		name   string
		scopes any // omitted when nil
		stored string
	}{
		{"omitted keeps the default", nil, `["*"]`},
		{"read", `["read"]`, `["read"]`},
		{"wildcard", `["*"]`, `["*"]`},
		{"padded and mixed are stored compact", `[ "read" , "pad:write" ]`, `["read","pad:write"]`},
		{"every vocabulary value", `["*","read","write","pad:read","pad:write","pad:admin"]`, `["*","read","write","pad:read","pad:write","pad:admin"]`},
	}
	// Not strings: the field itself is null, an array or a number. A JSON
	// null used to decode as "" and mint a full-access key (Codex r1).
	refusedRaw := []struct {
		name   string
		scopes json.RawMessage
		named  string
	}{
		{"a JSON null field", json.RawMessage(`null`), "got null"},
		{"an array, not a string", json.RawMessage(`["read"]`), `got ["read"]`},
		{"a number", json.RawMessage(`1`), "got 1"},
		{"a spaced JSON null field", json.RawMessage(" null "), "got null"},
		{"an empty string", json.RawMessage(`""`), "omit scopes"},
	}
	refused := []struct {
		name, scopes, named string
	}{
		{"null", `null`, `"null"`},
		{"unparseable", `read`, `"read"`},
		{"an object", `{}`, `"{}"`},
		{"a bare string", `"read"`, `"\"read\""`},
		{"a non-string element", `["read",1]`, `"[\"read\",1]"`},
		{"empty array", `[]`, "omit scopes"},
		{"padded empty array", `[ ]`, "omit scopes"},
		{"unknown value", `["read-only"]`, `"read-only"`},
		{"case matters", `["Read"]`, `"Read"`},
		{"unknown beside a known one", `["read","docs"]`, `"docs"`},
		{"null element", `["read",null]`, `""`},
	}

	for door, path := range doors {
		for i, tc := range accepted {
			body := map[string]any{"name": door + "-ok-" + string(rune('a'+i))}
			if tc.scopes != nil {
				body["scopes"] = tc.scopes
			}
			rr := doRequestWithBearer(env.srv, "POST", path, env.sessTok, body)
			if rr.Code != http.StatusCreated {
				t.Errorf("%s door, %s: %d %s, want 201", door, tc.name, rr.Code, rr.Body.String())
				continue
			}
			var got struct {
				Scopes string `json:"scopes"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Scopes != tc.stored {
				t.Errorf("%s door, %s: stored %q, want %q", door, tc.name, got.Scopes, tc.stored)
			}
		}
		for _, tc := range refused {
			rr := doRequestWithBearer(env.srv, "POST", path, env.sessTok,
				map[string]any{"name": door + "-bad", "scopes": tc.scopes})
			if rr.Code != http.StatusBadRequest || errorCode(t, rr) != "validation_error" {
				t.Errorf("%s door, %s: %d %s, want 400 validation_error", door, tc.name, rr.Code, rr.Body.String())
				continue
			}
			if !strings.Contains(rr.Body.String(), jsonEscapedForBody(tc.named)) {
				t.Errorf("%s door, %s: message does not name %s: %s", door, tc.name, tc.named, rr.Body.String())
			}
		}
	}

	for door, path := range doors {
		for _, tc := range refusedRaw {
			rr := doRequestWithBearer(env.srv, "POST", path, env.sessTok,
				map[string]any{"name": door + "-bad", "scopes": tc.scopes})
			if rr.Code != http.StatusBadRequest || errorCode(t, rr) != "validation_error" {
				t.Errorf("%s door, %s: %d %s, want 400 validation_error", door, tc.name, rr.Code, rr.Body.String())
				continue
			}
			if !strings.Contains(rr.Body.String(), jsonEscapedForBody(tc.named)) {
				t.Errorf("%s door, %s: message does not name %s: %s", door, tc.name, tc.named, rr.Body.String())
			}
		}
	}

	// Nothing refused was minted.
	tokens, err := env.srv.store.ListUserAPITokens(env.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		if strings.HasSuffix(tok.Name, "-bad") {
			t.Errorf("a refused create minted %q with scopes %q", tok.Name, tok.Scopes)
		}
	}
}

// jsonEscapedForBody is s as it appears inside a JSON string value, the
// form the error message takes in the response body.
func jsonEscapedForBody(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}
