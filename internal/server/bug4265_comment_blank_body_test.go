package server

import (
	"net/http"
	"testing"
)

// BUG-4265: a comment whose body is empty or only whitespace is refused with
// a 400 before anything is written, on the create route as on the edit and
// reply routes. The CLI refuses it first; this is the server's own answer,
// which the web UI and the HTTP MCP tools reach directly.
func TestCreateComment_RefusesABlankBody(t *testing.T) {
	t.Parallel()
	f := newAttributionFixture(t)
	path := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.item.Slug + "/comments"

	for name, body := range map[string]string{
		"empty":            "",
		"spaces":           "   ",
		"newlines and tab": "\n\t\n",
	} {
		t.Run(name, func(t *testing.T) {
			rr := doRequestWithCookie(f.srv, "POST", path, map[string]any{"body": body}, f.tok)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("POST blank comment = %d %s, want 400", rr.Code, rr.Body.String())
			}
		})
	}

	// Nothing was written by any of the refusals.
	list := doRequestWithCookie(f.srv, "GET", path, nil, f.tok)
	if list.Code != http.StatusOK {
		t.Fatalf("GET comments = %d %s", list.Code, list.Body.String())
	}
	if got := list.Body.String(); got != "[]\n" && got != "[]" {
		t.Errorf("comments after three refusals = %s, want none", got)
	}

	// Control: a real body is still accepted.
	ok := doRequestWithCookie(f.srv, "POST", path, map[string]any{"body": "a real comment"}, f.tok)
	if ok.Code/100 != 2 {
		t.Fatalf("control POST = %d %s, want 2xx", ok.Code, ok.Body.String())
	}
}
