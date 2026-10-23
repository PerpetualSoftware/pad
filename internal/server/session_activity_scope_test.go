package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestSessionActivityTokenScopes(t *testing.T) {
	srv := testServerWithPresence(t)
	_, _, _, user := setupWatchTestUser(t, srv)
	for _, scope := range []string{"read", "pad:read", "write"} {
		t.Run(scope, func(t *testing.T) {
			tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
				Name: scope, Scopes: `["` + scope + `"]`,
			}, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			rr := presenceRequest(t, srv, http.MethodPost, tok.Token, presenceWorkerA, `{"state":"busy"}`)
			if scope == "write" {
				if rr.Code != http.StatusNoContent {
					t.Fatalf("write token: %d %s; want 204", rr.Code, rr.Body.String())
				}
				return
			}
			var body struct {
				Error struct {
					Code, Message string
					Details       struct{ Reason string }
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rr.Code != http.StatusForbidden || body.Error.Code != "forbidden" || body.Error.Details.Reason != "token_scope" {
				t.Fatalf("read token: %d %s; want 403 forbidden for token_scope", rr.Code, rr.Body.String())
			}
		})
	}
}
