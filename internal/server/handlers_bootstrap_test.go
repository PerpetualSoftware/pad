package server

import (
	"encoding/json"
	"fmt"
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

	// Dashboard is populated when a request context is available;
	// recent_activity lives inside it (the top-level duplicate was
	// retired in PLAN-1410 / TASK-1413). On the empty-workspace path
	// dashboard may itself be nil if buildDashboardResponse returns
	// an empty/error result — the agent tolerates that via the
	// omitempty tag, so we don't assert non-nil here.
	if b.Dashboard != nil && b.Dashboard.RecentActivity == nil {
		t.Error("dashboard.recent_activity is nil; should be an empty slice")
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

	// PLAN-1410 / TASK-1413: the top-level `recent_activity` key was
	// retired (duplicate of dashboard.recent_activity). The remaining
	// top-level array fields must still serialize as [] not null.
	for _, key := range []string{"collections", "conventions", "roles", "playbooks"} {
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

	// Guard against accidental reintroduction of the duplicate
	// top-level recent_activity field.
	if _, ok := raw["recent_activity"]; ok {
		t.Errorf("top-level recent_activity reappeared in bootstrap response; it should live only under dashboard")
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

// bootstrapSizeBudget is the maximum byte count tolerated for the bootstrap
// JSON response against the seeded fixture in seedBootstrapSizeFixture.
//
// PLAN-1410 background: on the docapp production workspace, `pad bootstrap
// --format json` returns ~52,000 bytes / ~13,000 tokens, and the agent
// skill loads this blob on every `/pad` invocation. The plan trims the
// payload in stages (slim collection projection → drop duplicate
// recent_activity → cap dashboard arrays → drop convention slug → bump
// ToolSurfaceVersion to 0.4). This budget is the in-test ratchet that
// keeps later shape-change PRs honest: each one tightens this constant
// once the win is measured against the fixture.
//
// Budget history (each line is a PLAN-1410 PR that landed a win):
//
//	TASK-1411 — 16 KiB (baseline benchmark added; fixture at 13,861 bytes)
//	TASK-1412 — 11 KiB (slim BootstrapCollection projection; fixture at 8,992 bytes — collections section dropped from 8,848 to 3,979 bytes)
//	TASK-1413 — 8 KiB  (dedup top-level recent_activity, drop convention slug, cap dashboard.attention/recent_activity to 5 with overflow counts; fixture at 6,355 bytes — total dropped another 2,637 bytes)
//	TASK-1417 — 7 KiB  (close-out: bootstrap shape work complete, fixture still at 6,355 bytes; locks in the cumulative -54.2% win with ~12.8% headroom for routine schema reordering)
//
// PLAN-1410 followups land their own wins under their own budget
// ratchets (e.g. IDEA-1421's dashboard sub-array caps). The constant
// intentionally lives next to the test that consumes it so PRs
// touching the bootstrap shape see the budget in the diff.
const bootstrapSizeBudget = 7 * 1024

// TestBootstrapSizeBudget locks in a payload-size budget for the
// bootstrap response so future regressions are caught at PR time.
//
// The fixture approximates a small-but-realistic workspace: the
// default template seeds plus a handful of always-on conventions with
// realistic bodies, one slug-invocable playbook, and a spread of items
// across collections to populate the dashboard. Production workspaces
// (docapp had ~52KB at PLAN-1410's measurement) will exceed this
// fixture's bytes — but the contributors to the payload (per-collection
// schema/settings shape, per-convention body, dashboard caps,
// duplicated recent_activity) are exercised proportionally here, so a
// shape-side regression shows up at fixture scale.
//
// On failure, the per-section breakdown is logged so the regression's
// origin is obvious without re-running locally.
func TestBootstrapSizeBudget(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	seedBootstrapSizeFixture(t, srv, slug)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	total := rr.Body.Len()

	var b AgentBootstrap
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode bootstrap for breakdown: %v", err)
	}
	breakdown := bootstrapSectionBytes(b)

	// Always log the breakdown so passing runs leave a measurable
	// audit trail in CI output — that's how we track the trend as
	// PLAN-1410's shape-change PRs land.
	t.Logf("bootstrap size: %d bytes (budget %d)", total, bootstrapSizeBudget)
	for _, line := range breakdown {
		t.Logf("  %s", line)
	}

	if total > bootstrapSizeBudget {
		t.Errorf("bootstrap size %d bytes exceeds budget %d bytes — see per-section breakdown above. "+
			"If this is an intentional growth, raise bootstrapSizeBudget with a comment explaining why.",
			total, bootstrapSizeBudget)
	}
}

// seedBootstrapSizeFixture populates the workspace with the contributors
// the bootstrap size benchmark wants to exercise: always-on conventions
// with body content, a slug-invocable playbook (the user-callable shape),
// and a handful of items across collections to populate the dashboard.
//
// Keep this in sync with bootstrapSizeBudget — adding content here
// without raising the budget will fail TestBootstrapSizeBudget.
func seedBootstrapSizeFixture(t *testing.T, srv *Server, wsSlug string) {
	t.Helper()

	createItem(t, srv, wsSlug, "conventions", map[string]interface{}{
		"title":   "Run tests before commit",
		"content": "Run `make check` before every commit. CI runs the same suite locally; catching failures here saves a round-trip and keeps main green.",
		"fields":  `{"status":"active","trigger":"always","priority":"must","scope":"all"}`,
	})
	createItem(t, srv, wsSlug, "conventions", map[string]interface{}{
		"title":   "Prefer composition over inheritance",
		"content": "When extending behavior, embed and compose rather than subclass. Easier to test, easier to refactor when requirements change, no surprise dispatch.",
		"fields":  `{"status":"active","trigger":"always","priority":"should","scope":"backend"}`,
	})

	createItem(t, srv, wsSlug, "playbooks", map[string]interface{}{
		"title":   "Cut a release",
		"content": "Release the next version.\n\n## Arguments\n\n- version (string, required) — semver, e.g. 0.5.0\n\n## Steps\n\n1. Verify clean tree on main\n2. Tag and push\n3. Verify CI release workflow",
		"fields":  `{"status":"active","trigger":"manual","invocation_slug":"release","arguments":[{"name":"version","type":"string","required":true}]}`,
	})

	for i := 0; i < 5; i++ {
		createItem(t, srv, wsSlug, "tasks", map[string]interface{}{
			"title":   fmt.Sprintf("Sample task %d", i),
			"content": "Task body — placeholder content to give the dashboard something to summarize.",
			"fields":  `{"status":"open","priority":"medium"}`,
		})
	}
	createItem(t, srv, wsSlug, "plans", map[string]interface{}{
		"title":   "Sample plan",
		"content": "An active plan with a few children. The dashboard counts it as active work.",
		"fields":  `{"status":"active"}`,
	})
}

// bootstrapSectionBytes returns a per-section byte breakdown of a
// bootstrap blob. Marshalled with the same encoder behavior as the
// real handler (compact, no indent) so the per-section totals sum
// close to the overall response body length, modulo the top-level
// JSON object's structural overhead.
//
// Diagnostic only — not part of any production contract. Sized for
// readability in `go test -v` output.
func bootstrapSectionBytes(b AgentBootstrap) []string {
	lines := []string{
		fmt.Sprintf("workspace:   %d bytes", jsonLen(b.Workspace)),
		fmt.Sprintf("user:        %d bytes", jsonLen(b.User)),
		fmt.Sprintf("collections: %d bytes (%d items)", jsonLen(b.Collections), len(b.Collections)),
		fmt.Sprintf("conventions: %d bytes (%d items)", jsonLen(b.Conventions), len(b.Conventions)),
		fmt.Sprintf("roles:       %d bytes (%d items)", jsonLen(b.Roles), len(b.Roles)),
		fmt.Sprintf("playbooks:   %d bytes (%d items)", jsonLen(b.Playbooks), len(b.Playbooks)),
		fmt.Sprintf("dashboard:   %d bytes", jsonLen(b.Dashboard)),
	}
	// Surface the caps' effect when triggered so the trim's value is
	// legible from CI output as PLAN-1410's later PRs land.
	if b.Dashboard != nil {
		if b.Dashboard.AttentionOverflowCount > 0 {
			lines = append(lines, fmt.Sprintf(
				"  └─ attention capped: %d shown, %d overflow",
				len(b.Dashboard.Attention), b.Dashboard.AttentionOverflowCount))
		}
		if b.Dashboard.RecentActivityOverflowCount > 0 {
			lines = append(lines, fmt.Sprintf(
				"  └─ recent_activity capped: %d shown, %d overflow",
				len(b.Dashboard.RecentActivity), b.Dashboard.RecentActivityOverflowCount))
		}
	}
	return lines
}

// jsonLen marshals v to compact JSON and returns the byte count. Test
// helper; errors are squashed because they'd indicate a programming
// error in the test (un-marshalable value) and we want the
// per-section breakdown to render even if one slice fails to encode.
func jsonLen(v interface{}) int {
	out, err := json.Marshal(v)
	if err != nil {
		return -1
	}
	return len(out)
}

// TestCapBootstrapDashboard isolates the bootstrap dashboard cap logic
// from the rest of the bootstrap pipeline so the contract (cap to N,
// surface overflow count, leave the source pointer untouched) doesn't
// drift silently. PLAN-1410 / TASK-1413.
func TestCapBootstrapDashboard(t *testing.T) {
	// Helper: make a DashboardResponse with N attention + M recent_activity.
	mk := func(attN, recN int) *DashboardResponse {
		attention := make([]DashboardAttention, attN)
		for i := range attention {
			attention[i] = DashboardAttention{Type: "stalled", ItemRef: fmt.Sprintf("TASK-%d", i)}
		}
		recent := make([]DashboardActivity, recN)
		for i := range recent {
			recent[i] = DashboardActivity{Action: "updated", ItemSlug: fmt.Sprintf("item-%d", i)}
		}
		return &DashboardResponse{Attention: attention, RecentActivity: recent}
	}

	t.Run("under-cap-no-overflow", func(t *testing.T) {
		d := mk(2, 3)
		out := capBootstrapDashboard(d)
		if out == nil {
			t.Fatal("expected non-nil result even when nothing trimmed")
		}
		if got := len(out.Attention); got != 2 {
			t.Errorf("attention len = %d, want 2 (under cap, no trim)", got)
		}
		if out.AttentionOverflowCount != 0 {
			t.Errorf("attention_overflow_count = %d, want 0", out.AttentionOverflowCount)
		}
		if got := len(out.RecentActivity); got != 3 {
			t.Errorf("recent_activity len = %d, want 3", got)
		}
		if out.RecentActivityOverflowCount != 0 {
			t.Errorf("recent_activity_overflow_count = %d, want 0", out.RecentActivityOverflowCount)
		}
	})

	t.Run("over-cap-truncates-and-counts-overflow", func(t *testing.T) {
		d := mk(bootstrapAttentionCap+8, bootstrapRecentActivityCap+3)
		out := capBootstrapDashboard(d)
		if got := len(out.Attention); got != bootstrapAttentionCap {
			t.Errorf("attention len = %d, want %d (capped)", got, bootstrapAttentionCap)
		}
		if out.AttentionOverflowCount != 8 {
			t.Errorf("attention_overflow_count = %d, want 8", out.AttentionOverflowCount)
		}
		if got := len(out.RecentActivity); got != bootstrapRecentActivityCap {
			t.Errorf("recent_activity len = %d, want %d (capped)", got, bootstrapRecentActivityCap)
		}
		if out.RecentActivityOverflowCount != 3 {
			t.Errorf("recent_activity_overflow_count = %d, want 3", out.RecentActivityOverflowCount)
		}
	})

	t.Run("source-pointer-unchanged", func(t *testing.T) {
		// Defensive contract: callers downstream of buildDashboardResponse
		// (the dashboard endpoint itself) must see their full-length
		// arrays. The cap mutates a shallow copy.
		d := mk(bootstrapAttentionCap+5, bootstrapRecentActivityCap+5)
		origAttLen := len(d.Attention)
		origRecLen := len(d.RecentActivity)

		_ = capBootstrapDashboard(d)

		if got := len(d.Attention); got != origAttLen {
			t.Errorf("source Attention mutated: len = %d, want %d", got, origAttLen)
		}
		if got := len(d.RecentActivity); got != origRecLen {
			t.Errorf("source RecentActivity mutated: len = %d, want %d", got, origRecLen)
		}
	})

	t.Run("exact-cap-no-overflow", func(t *testing.T) {
		// Boundary: len == cap should not flag overflow.
		d := mk(bootstrapAttentionCap, bootstrapRecentActivityCap)
		out := capBootstrapDashboard(d)
		if out.AttentionOverflowCount != 0 {
			t.Errorf("attention_overflow_count at exact cap = %d, want 0", out.AttentionOverflowCount)
		}
		if out.RecentActivityOverflowCount != 0 {
			t.Errorf("recent_activity_overflow_count at exact cap = %d, want 0", out.RecentActivityOverflowCount)
		}
	})
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
