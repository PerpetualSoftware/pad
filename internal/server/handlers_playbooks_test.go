package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// TestPlaybookList verifies the list endpoint returns the metadata shape
// agents expect (slug + invocation_slug + has_arguments). This is the
// same projection bootstrap uses; the dedicated endpoint lets callers
// skip the rest of the bootstrap blob when they only need the catalog.
func TestPlaybookList(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Cut release",
		"content": "Step 1: tag",
		"fields":  `{"status":"active","trigger":"on-release","invocation_slug":"release","arguments":[{"name":"version","type":"string","required":true}]}`,
	})
	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Triage bugs",
		"content": "Auto-loaded — no slug",
		"fields":  `{"status":"active","trigger":"on-triage"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list playbooks: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var list []AgentBootstrapPlaybookMeta
	parseJSON(t, rr, &list)

	if len(list) < 2 {
		t.Fatalf("expected 2 playbooks, got %d", len(list))
	}
	// invocation_slug-bearing playbook should sort first (see
	// collectPlaybookMetadata's stable sort).
	if list[0].InvocationSlug != "release" {
		t.Errorf("expected invocation_slug='release' first, got %q (full list: %#v)", list[0].InvocationSlug, list)
	}
	if !list[0].HasArguments {
		t.Errorf("expected has_arguments=true for the release playbook")
	}
}

// TestPlaybookShowByInvocationSlug verifies the invocation_slug
// resolution path — `pad playbook show ship` should find the playbook
// regardless of its underlying ref or item slug.
func TestPlaybookShowByInvocationSlug(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Ship something",
		"content": "Step 1",
		"fields":  `{"status":"active","invocation_slug":"ship"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks/ship", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("show by invocation_slug: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var item map[string]any
	parseJSON(t, rr, &item)
	if title, _ := item["title"].(string); title != "Ship something" {
		t.Errorf("title = %v, want Ship something", item["title"])
	}
}

// TestPlaybookShowByRef verifies the fallback resolution — when the
// identifier isn't an invocation_slug, fall through to ResolveItem
// (UUID / ref / item slug).
func TestPlaybookShowByRef(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	created := createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":  "Just a playbook",
		"fields": `{"status":"active"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks/"+created.Ref, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("show by ref: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var item map[string]any
	parseJSON(t, rr, &item)
	if ref, _ := item["ref"].(string); ref != created.Ref {
		t.Errorf("ref = %v, want %s", item["ref"], created.Ref)
	}
}

// TestPlaybookShowRejectsNonPlaybook verifies the resolver refuses to
// surface non-playbook items even when their ref matches — keeps
// /pad <slug> routing from accidentally picking up a TASK.
func TestPlaybookShowRejectsNonPlaybook(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	task := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "A normal task",
		"fields": `{"status":"open"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks/"+task.Ref, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("show non-playbook by ref: expected 404, got %d", rr.Code)
	}
}

// TestPlaybookRunBindsArgs verifies the run endpoint parses the
// `arguments` spec from fields, binds the caller's supplied values,
// and surfaces required-but-missing args in the unbound list (so the
// agent can prompt the user rather than failing the call).
func TestPlaybookRunBindsArgs(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "Ship",
		"content": "Step 1",
		"fields": `{"status":"active","invocation_slug":"ship","arguments":[
			{"name":"target","type":"ref","required":true},
			{"name":"merge-strategy","type":"enum","default":"squash","enum":["squash","merge","rebase"]},
			{"name":"stop-after-each","type":"flag"},
			{"name":"limit","type":"number"}
		]}`,
	})

	t.Run("with-args", func(t *testing.T) {
		body := map[string]any{
			"args": map[string]any{
				"target":          "PLAN-7",
				"stop-after-each": true,
				"limit":           float64(3),
			},
		}
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/ship/run", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("run: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp PlaybookRunResponse
		parseJSON(t, rr, &resp)
		if resp.BoundArgs["target"] != "PLAN-7" {
			t.Errorf("bound_args[target] = %v, want PLAN-7", resp.BoundArgs["target"])
		}
		if resp.BoundArgs["stop-after-each"] != true {
			t.Errorf("bound_args[stop-after-each] = %v, want true", resp.BoundArgs["stop-after-each"])
		}
		// merge-strategy should default to 'squash' since the caller
		// didn't supply it.
		if resp.BoundArgs["merge-strategy"] != "squash" {
			t.Errorf("bound_args[merge-strategy] = %v, want squash (default)", resp.BoundArgs["merge-strategy"])
		}
	})

	t.Run("missing-required-surfaces-unbound", func(t *testing.T) {
		body := map[string]any{
			"args": map[string]any{}, // no target
		}
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/ship/run", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("run: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp PlaybookRunResponse
		parseJSON(t, rr, &resp)
		if len(resp.Unbound) != 1 || resp.Unbound[0].Name != "target" {
			t.Errorf("unbound list should contain 'target'; got %#v", resp.Unbound)
		}
	})

	t.Run("with-raw-args", func(t *testing.T) {
		body := map[string]any{
			"raw_args": []string{"PLAN-7", "merge-strategy=rebase", "stop-after-each"},
		}
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/ship/run", body)
		if rr.Code != http.StatusOK {
			t.Fatalf("run raw_args: expected 200, got %d: %s", rr.Code, rr.Body.String())
		}
		var resp PlaybookRunResponse
		parseJSON(t, rr, &resp)
		// target is the first declared positional, so it should pick up PLAN-7
		if resp.BoundArgs["target"] != "PLAN-7" {
			t.Errorf("bound_args[target] = %v, want PLAN-7 (positional)", resp.BoundArgs["target"])
		}
		if resp.BoundArgs["merge-strategy"] != "rebase" {
			t.Errorf("bound_args[merge-strategy] = %v, want rebase (key=value)", resp.BoundArgs["merge-strategy"])
		}
		if resp.BoundArgs["stop-after-each"] != true {
			t.Errorf("bound_args[stop-after-each] = %v, want true (flag)", resp.BoundArgs["stop-after-each"])
		}
	})
}

// TestParsePlaybookCLIArgsErrors verifies the strict parser surfaces
// the right errors for each malformed-input class. The CLI relays
// these to the user verbatim, so the messages need to be specific.
func TestParsePlaybookCLIArgsErrors(t *testing.T) {
	specs := []PlaybookArgumentSpec{
		{Name: "target", Type: "ref", Required: true},
		{Name: "merge-strategy", Type: "enum", Enum: []string{"squash", "merge"}},
		{Name: "flag-thing", Type: "flag"},
	}

	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown-key", []string{"foo=bar"}, `unknown argument "foo"`},
		{"flag-as-key-value", []string{"flag-thing=true"}, "flag-thing"},
		{"bad-enum", []string{"merge-strategy=tomato"}, `must be one of [squash merge]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePlaybookCLIArgs(tc.args, specs)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}

	// Separate fixture: only one positional slot so the
	// "no remaining positional slots" branch is reachable.
	tightSpecs := []PlaybookArgumentSpec{
		{Name: "target", Type: "ref", Required: true},
	}
	if _, err := ParsePlaybookCLIArgs([]string{"PLAN-1", "PLAN-2"}, tightSpecs); err == nil {
		t.Fatal("expected error for excess positional with one declared slot")
	} else if !contains(err.Error(), "no remaining positional slots") {
		t.Errorf("error = %q, want 'no remaining positional slots'", err.Error())
	}
}

// TestPlaybookRunAcceptsEmptyBody verifies that POSTing the run
// endpoint with no body (or only a {} body) still works for playbooks
// with no required args — the previous EOF-string check rejected
// the legitimate no-args call.
func TestPlaybookRunAcceptsEmptyBody(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createItem(t, srv, slug, "playbooks", map[string]interface{}{
		"title":   "No-args playbook",
		"content": "Just steps",
		"fields":  `{"status":"active","invocation_slug":"no-args"}`,
	})

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/playbooks/no-args/run", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty-body run: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestParsePlaybookCLIArgsOptionalNotPositional verifies the strict
// rule that ONLY required args are positional. With an optional arg
// declared before a required one, a bareword token must skip past
// the optional and fill the required slot.
func TestParsePlaybookCLIArgsOptionalNotPositional(t *testing.T) {
	specs := []PlaybookArgumentSpec{
		{Name: "merge-strategy", Type: "enum", Enum: []string{"squash", "rebase"}}, // optional, first
		{Name: "target", Type: "ref", Required: true},                              // required, second
	}
	parsed, err := ParsePlaybookCLIArgs([]string{"TASK-7"}, specs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed["target"] != "TASK-7" {
		t.Errorf("expected target=TASK-7 (skipping the optional first slot); got %#v", parsed)
	}
	if _, ok := parsed["merge-strategy"]; ok {
		t.Errorf("merge-strategy should not be positionally filled; got %v", parsed["merge-strategy"])
	}
}

// TestCoercePlaybookValueNumberRejectsBadInput verifies the number
// coercion rejects partial tokens, NaN, and Inf — Codex round 1
// caught Sscanf("%g") accepting "1abc" silently.
func TestCoercePlaybookValueNumberRejectsBadInput(t *testing.T) {
	spec := PlaybookArgumentSpec{Name: "limit", Type: "number"}

	good := []struct {
		in   string
		want float64
	}{
		{"3", 3},
		{"3.5", 3.5},
		{"-2", -2},
	}
	for _, tc := range good {
		v, err := coercePlaybookValue(tc.in, spec)
		if err != nil {
			t.Errorf("expected %q to parse, got error: %v", tc.in, err)
			continue
		}
		if v != tc.want {
			t.Errorf("parse %q = %v, want %v", tc.in, v, tc.want)
		}
	}

	bad := []string{"1abc", "abc", "", "NaN", "Inf", "-Inf", "+Inf"}
	for _, in := range bad {
		if _, err := coercePlaybookValue(in, spec); err == nil {
			t.Errorf("expected %q to fail parsing, got success", in)
		}
	}
}

// contains is a small test helper.
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestPlaybookListEmptyShape verifies a workspace with no playbooks
// returns an empty array rather than null, matching bootstrap's
// contract.
func TestPlaybookListEmptyShape(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/playbooks", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list empty: expected 200, got %d", rr.Code)
	}
	// Should decode to an empty slice, never null.
	var raw json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if string(raw) == "null" {
		t.Errorf("empty list serialized as null; want []")
	}
	var list []AgentBootstrapPlaybookMeta
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if !reflect.DeepEqual(list, []AgentBootstrapPlaybookMeta{}) && len(list) != 0 {
		t.Errorf("expected empty list, got %v", list)
	}
}
