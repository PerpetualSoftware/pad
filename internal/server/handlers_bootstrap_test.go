package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestBootstrapEmptyWorkspace verifies the bootstrap blob returns the
// scaffolding for a workspace with no items beyond the template seeds.
// The /pad skill relies on this single call replacing four separate
// context-loading calls; any of the expected keys missing would silently
// break greeting behavior.
func TestBootstrapEmptyWorkspace(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var b AgentBootstrap
	parseJSON(t, rr, &b)

	if b.Workspace.Slug != slug {
		t.Errorf("workspace.slug = %q, want %q", b.Workspace.Slug, slug)
	}
	if b.Workspace.Name == "" {
		t.Error("workspace.name empty")
	}

	if len(b.Collections) == 0 {
		t.Error("collections empty — template seed must produce at least Tasks/Ideas/Plans/Docs/Conventions/Playbooks")
	}

	// Roles is non-nil by contract — agents iterate it without nil-checks.
	if b.Roles == nil {
		t.Error("roles is nil; should be an empty slice")
	}

	// RecentActivity is non-nil by contract.
	if b.RecentActivity == nil {
		t.Error("recent_activity is nil; should be an empty slice")
	}
}

// TestBootstrapEmptyArraysNotNull verifies the JSON wire shape: arrays
// must serialize as [] not null so the agent skill can iterate without
// defensive nil checks.
func TestBootstrapEmptyArraysNotNull(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}

	for _, key := range []string{"collections", "conventions", "roles", "playbooks", "recent_activity"} {
		val, ok := raw[key]
		if !ok {
			t.Errorf("missing key %q in bootstrap response", key)
			continue
		}
		s := string(val)
		if s == "null" {
			t.Errorf("bootstrap.%s serialized as null; want []", key)
		}
	}
}

// TestBootstrapIncludesPlaybookMetadata verifies that a seeded playbook
// item flows into the bootstrap's playbooks array with the right
// projection — slug, invocation_slug, has_arguments — without leaking
// the body (which is intentionally omitted from bootstrap for size).
func TestBootstrapIncludesPlaybookMetadata(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create a playbook with an invocation_slug + arguments.
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Test playbook",
		"content": "First paragraph — used as the summary.\n\nSecond paragraph (ignored).",
		"fields":  `{"status":"active","trigger":"manual","invocation_slug":"test-bp","arguments":[{"name":"target","type":"ref","required":true}]}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var b AgentBootstrap
	parseJSON(t, rr, &b)

	var found *AgentBootstrapPlaybookMeta
	for i := range b.Playbooks {
		if b.Playbooks[i].InvocationSlug == "test-bp" {
			found = &b.Playbooks[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("bootstrap.playbooks missing test-bp; got %+v", b.Playbooks)
	}
	if found.Title != "Test playbook" {
		t.Errorf("playbook.title = %q, want %q", found.Title, "Test playbook")
	}
	if !found.HasArguments {
		t.Error("playbook.has_arguments = false, want true (arguments list was non-empty)")
	}
	if found.Summary == "" {
		t.Error("playbook.summary empty; expected the first body paragraph")
	}
}

// TestPlaybookSummaryPrefersFirstParagraph isolates the summary extraction
// from the bootstrap path so the rule (skip headings, take first non-empty
// paragraph, cap at ~240 chars) doesn't drift silently.
func TestPlaybookSummaryPrefersFirstParagraph(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "skips-headings",
			body: "# Title\n\n## Overview\n\nThis is the first prose line.",
			want: "This is the first prose line.",
		},
		{
			name: "trims-leading-whitespace",
			body: "   Indented summary line.",
			want: "Indented summary line.",
		},
		{
			name: "empty-body",
			body: "",
			want: "",
		},
		{
			name: "headings-only",
			body: "# A\n## B\n### C",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := playbookSummary(tc.body)
			if got != tc.want {
				t.Errorf("playbookSummary() = %q, want %q", got, tc.want)
			}
		})
	}

	// Long bodies must be truncated. Verify capping puts an ellipsis on
	// the end so callers can detect truncation visually.
	long := ""
	for i := 0; i < 100; i++ {
		long += "abcdefghij"
	}
	got := playbookSummary(long)
	if len(got) > 240 {
		t.Errorf("long summary not capped at 240 chars; got %d", len(got))
	}
	if got[len(got)-len("…"):] != "…" {
		t.Errorf("truncated summary missing ellipsis: %q", got)
	}
}
