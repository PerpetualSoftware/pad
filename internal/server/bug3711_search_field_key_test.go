package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3711: GET /api/v1/search must accept every field key allowed by the
// shared rule (store.IsValidFieldKey, including underscore/digit/hyphen leads)
// and refuse an unsupported or empty field.* key with 400 validation_error
// instead of silently dropping it and widening the results.
func TestBUG3711_SearchFieldFilterKeyValidation(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	wsSlug := createWSForTest(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(wsSlug)
	if err != nil || ws == nil {
		t.Fatalf("workspace %q: %v", wsSlug, err)
	}
	tasks, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("tasks collection: %v", err)
	}

	if _, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{
		Title:  "Fix ops bug",
		Fields: `{"status":"open","_team":"ops","9lives":"cat","-x":"yes"}`,
	}); err != nil {
		t.Fatalf("create ops item: %v", err)
	}
	if _, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{
		Title:  "Fix web bug",
		Fields: `{"status":"open","_team":"web","9lives":"dog","-x":"no"}`,
	}); err != nil {
		t.Fatalf("create web item: %v", err)
	}

	type errorEnvelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	t.Run("underscore key narrows search results", func(t *testing.T) {
		rr := doRequest(srv, "GET", "/api/v1/search?q=bug&workspace="+ws.Slug+"&field._team=ops", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
		}
		var resp store.SearchResponse
		parseJSON(t, rr, &resp)
		if len(resp.Results) != 1 {
			t.Fatalf("got %d results, want 1", len(resp.Results))
		}
		if resp.Results[0].Item.Title != "Fix ops bug" {
			t.Errorf("got title %q, want %q", resp.Results[0].Item.Title, "Fix ops bug")
		}
	})

	t.Run("digit and hyphen leading keys narrow search results", func(t *testing.T) {
		for _, param := range []string{"field.9lives=cat", "field.-x=yes"} {
			rr := doRequest(srv, "GET", "/api/v1/search?q=bug&workspace="+ws.Slug+"&"+param, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200: %s", param, rr.Code, rr.Body.String())
			}
			var resp store.SearchResponse
			parseJSON(t, rr, &resp)
			if len(resp.Results) != 1 || resp.Results[0].Item.Title != "Fix ops bug" {
				t.Errorf("%s: got %d results, want only 'Fix ops bug'", param, len(resp.Results))
			}
		}
	})

	t.Run("unsafe key refused with 400 validation_error", func(t *testing.T) {
		rr := doRequest(srv, "GET", "/api/v1/search?q=bug&workspace="+ws.Slug+"&field.a%20b=x", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
		}
		var env errorEnvelope
		parseJSON(t, rr, &env)
		if env.Error.Code != "validation_error" {
			t.Errorf("code = %q, want validation_error", env.Error.Code)
		}
		if !strings.HasPrefix(env.Error.Message, "invalid search filter") {
			t.Errorf("message = %q, want prefix 'invalid search filter'", env.Error.Message)
		}
	})

	t.Run("empty key with non-empty value refused with 400 validation_error", func(t *testing.T) {
		rr := doRequest(srv, "GET", "/api/v1/search?q=bug&workspace="+ws.Slug+"&field.=x", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
		}
		var env errorEnvelope
		parseJSON(t, rr, &env)
		if env.Error.Code != "validation_error" {
			t.Errorf("code = %q, want validation_error", env.Error.Code)
		}
		if !strings.HasPrefix(env.Error.Message, "invalid search filter") {
			t.Errorf("message = %q, want prefix 'invalid search filter'", env.Error.Message)
		}
	})

	t.Run("multiple bad keys report sorted key deterministically", func(t *testing.T) {
		rr := doRequest(srv, "GET", "/api/v1/search?q=bug&workspace="+ws.Slug+"&field.b%20b=x&field.a%20a=y", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
		}
		var env errorEnvelope
		parseJSON(t, rr, &env)
		if env.Error.Code != "validation_error" {
			t.Errorf("code = %q, want validation_error", env.Error.Code)
		}
		if !strings.HasPrefix(env.Error.Message, "invalid search filter") {
			t.Errorf("message = %q, want prefix 'invalid search filter'", env.Error.Message)
		}
		if !strings.Contains(env.Error.Message, `"a a"`) {
			t.Errorf("message = %q, want it to name \"a a\"", env.Error.Message)
		}
	})

	t.Run("empty value for field param is still skipped", func(t *testing.T) {
		rr := doRequest(srv, "GET", "/api/v1/search?q=bug&workspace="+ws.Slug+"&field.a%20b=", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
		}
	})
}
