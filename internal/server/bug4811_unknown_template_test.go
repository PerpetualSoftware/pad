package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// BUG-4811: an unknown template name is the caller's typo, not a server
// fault. It used to reach the seed step after the workspace row existed, so
// the create minted a workspace, removed it again and answered 500
// "Failed to seed collections". It is now a 400 before anything is minted.
// internal/mcp's TestBUG4811_UnknownTemplateClassifiesAsValidation pins the
// message's stdio classification.
func TestBUG4811_UnknownTemplateIs400BeforeTheMint(t *testing.T) {
	srv, token := compensationServer(t)
	logs := captureLogs(t)
	before := workspaceCount(t, srv)

	body := map[string]string{"name": "Mobile", "template": "software"}
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", body, token)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400. body: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if env.Error.Code != "bad_request" {
		t.Errorf("code %q, want bad_request", env.Error.Code)
	}
	msg := env.Error.Message
	if !strings.HasPrefix(msg, `invalid template "software": must be one of `) {
		t.Errorf("message %q does not name the bad template", msg)
	}
	// The message lists the pickable templates, and only those.
	if !strings.Contains(msg, "startup") || !strings.Contains(msg, "scrum") {
		t.Errorf("message %q does not list the valid templates", msg)
	}
	if strings.Contains(msg, "demo") {
		t.Errorf("message %q lists a hidden template", msg)
	}

	if got := workspaceCount(t, srv); got != before {
		t.Fatalf("workspace count %d -> %d, want unchanged", before, got)
	}
	// Nothing was minted, so nothing was seeded or compensated.
	if strings.Contains(logs.String(), "workspace seeding failed") ||
		strings.Contains(logs.String(), "removing the workspace nobody could administer") {
		t.Fatalf("the refusal reached the mint: %s", logs.String())
	}

	// The same name is free: the refusal held nothing.
	rr = createWorkspaceAs(t, srv, token, "Mobile")
	if rr.Code != http.StatusCreated {
		t.Fatalf("retry status %d, want 201. body: %s", rr.Code, rr.Body.String())
	}
}

// The empty name (no starter pack) and a hidden template stay valid: the
// check accepts exactly what the seed step accepts.
func TestBUG4811_EmptyAndHiddenTemplatesStillCreate(t *testing.T) {
	srv, token := compensationServer(t)
	for _, c := range []struct{ name, template string }{
		{"No Pack", ""},
		{"Blank One", "blank"},
		{"Startup One", "startup"},
	} {
		body := map[string]string{"name": c.name, "template": c.template}
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", body, token)
		if rr.Code != http.StatusCreated {
			t.Errorf("template %q: status %d, want 201. body: %s", c.template, rr.Code, rr.Body.String())
		}
	}
}

func TestBUG4811_ValidateWorkspaceTemplate(t *testing.T) {
	for _, ok := range []string{"", "startup", "blank", "demo"} {
		if err := validateWorkspaceTemplate(ok); err != nil {
			t.Errorf("%q: %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{"software", "Startup", " startup", "no-such-template-exists"} {
		if err := validateWorkspaceTemplate(bad); err == nil {
			t.Errorf("%q accepted, want refused", bad)
		}
	}
}
