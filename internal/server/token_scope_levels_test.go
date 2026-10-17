package server

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// TestTokenScopeLevelsFixture holds tokenScopeAllows to the level each row
// of testdata/token_scope_levels.json claims (TASK-3866). The web's token
// list labels keys from the same file (web/src/lib/settings/tokenScopes.test.ts),
// so a change to the server's check that the label does not follow turns
// one of the two red.
func TestTokenScopeLevelsFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/token_scope_levels.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Rows []struct {
			Scopes string `json:"scopes"`
			Level  string `json:"level"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Rows) == 0 {
		t.Fatal("fixture has no rows")
	}
	for _, row := range fixture.Rows {
		read := tokenScopeAllows(row.Scopes, http.MethodGet, "/api/v1/workspaces")
		write := tokenScopeAllows(row.Scopes, http.MethodPatch, "/api/v1/workspaces")
		got := "none"
		switch {
		case read && write:
			got = "write"
		case read:
			got = "read"
		case write:
			got = "write-without-read"
		}
		if got != row.Level {
			t.Errorf("scopes %q: server grants %s, fixture says %s", row.Scopes, got, row.Level)
		}
	}
}
