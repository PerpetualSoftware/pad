package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
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

// TestBootstrapRoleProjection verifies the BootstrapRole wire shape
// after PLAN-1410 / TASK-1423 trimmed it. Seeds one role and asserts:
//
//   - The projection's expected fields (slug/name/description/icon/
//     item_count/sort_order) are present and correct.
//   - The dropped fields (id, workspace_id, tools, created_at,
//     updated_at) are NOT present in the marshalled JSON.
//
// The negative check is the load-bearing part — without it, a future
// refactor that "fixes" the projection by re-adding a UUID would
// pass all the positive assertions silently. Mirrors the contract
// that TestBootstrapEmptyArraysNotNull guards on the dedup side.
func TestBootstrapRoleProjection(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create a role via the agent-roles endpoint so the workspace
	// has one populated entry to project.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/agent-roles", map[string]interface{}{
		"name":        "Planner",
		"description": "Breaks down ideas, designs approaches",
		"icon":        "🧠",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create role: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Fetch the bootstrap and unmarshal into a permissive shape so we
	// can also detect the presence of fields that should NOT be there.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var raw struct {
		Roles []map[string]json.RawMessage `json:"roles"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode roles: %v", err)
	}
	if len(raw.Roles) != 1 {
		t.Fatalf("expected exactly 1 role, got %d", len(raw.Roles))
	}
	role := raw.Roles[0]

	// Positive: required projection fields are present.
	want := map[string]string{
		"slug":        `"planner"`,
		"name":        `"Planner"`,
		"description": `"Breaks down ideas, designs approaches"`,
		"item_count":  `0`,
	}
	for key, expected := range want {
		got, ok := role[key]
		if !ok {
			t.Errorf("missing required projection field %q", key)
			continue
		}
		if string(got) != expected {
			t.Errorf("role.%s = %s, want %s", key, string(got), expected)
		}
	}

	// Negative: dropped fields must NOT appear.
	for _, key := range []string{"id", "workspace_id", "tools", "created_at", "updated_at"} {
		if _, ok := role[key]; ok {
			t.Errorf("role.%s leaked into the bootstrap projection; should have been dropped by TASK-1423", key)
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
//	TASK-1422 — 9 KiB  (extend dashboard caps to active_items/active_plans/by_role; fixture grew to 7,823 bytes after seeding 6 in_progress tasks to exercise the new active_items cap — the growth is fixture-side, not shape-side, and the new caps demonstrably trigger in the per-section breakdown. suggested_next deliberately excluded — already capped to 3 upstream in buildDashboardResponse.)
//
// Note that the TASK-1422 budget loosening is purely fixture-side: the
// fixture deliberately seeds more `in_progress` items so the
// active_items cap fires under realistic load. The cap itself is
// purely a SAVINGS (clamps unbounded growth in live workspaces). On
// docapp the cap drops active_items from 7 → 5 entries.
//
// The constant intentionally lives next to the test that consumes it
// so PRs touching the bootstrap shape see the budget in the diff.
const bootstrapSizeBudget = 9 * 1024

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

	// 6 in-progress tasks: 6 > bootstrapActiveItemsCap (5) → exercises
	// the active_items cap in the fixture, producing an "active_items
	// capped: 5 shown, 1 overflow" line in the per-section breakdown.
	// in_progress (not open) because dashboard.active_items filters on
	// isActiveStatus(), which excludes initial/terminal statuses.
	for i := 0; i < 6; i++ {
		createItem(t, srv, wsSlug, "tasks", map[string]interface{}{
			"title":   fmt.Sprintf("Sample task %d", i),
			"content": "Task body — placeholder content to give the dashboard something to summarize.",
			"fields":  `{"status":"in-progress","priority":"medium"}`,
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
	// legible from CI output as PLAN-1410's PRs land. Each line follows
	// the same shape: "<array> capped: N shown, M overflow".
	if b.Dashboard != nil {
		type capLine struct {
			name     string
			shown    int
			overflow int
		}
		for _, c := range []capLine{
			{"attention", len(b.Dashboard.Attention), b.Dashboard.AttentionOverflowCount},
			{"recent_activity", len(b.Dashboard.RecentActivity), b.Dashboard.RecentActivityOverflowCount},
			{"active_items", len(b.Dashboard.ActiveItems), b.Dashboard.ActiveItemsOverflowCount},
			{"active_plans", len(b.Dashboard.ActivePlans), b.Dashboard.ActivePlansOverflowCount},
			{"by_role", len(b.Dashboard.ByRole), b.Dashboard.ByRoleOverflowCount},
		} {
			if c.overflow > 0 {
				lines = append(lines, fmt.Sprintf(
					"  └─ %s capped: %d shown, %d overflow",
					c.name, c.shown, c.overflow))
			}
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
// from the rest of the bootstrap pipeline so the contract (cap to N per
// array, surface overflow count, leave the source pointer untouched)
// doesn't drift silently as new caps are added.
//
// PLAN-1410 introduced caps on attention + recent_activity (TASK-1413)
// and extended them to active_items / active_plans / by_role
// (TASK-1422, absorbing IDEA-1421). `suggested_next` is deliberately
// excluded — it's already capped to 3 upstream in buildDashboardResponse,
// making a bootstrap-side cap unreachable in production. This test
// covers all five live caps under the same contract.
func TestCapBootstrapDashboard(t *testing.T) {
	// dashCounts is the per-array size knob the test uses to construct
	// a DashboardResponse with arbitrary fill levels. Each field can be
	// set independently so subtests can exercise specific caps without
	// populating the others — keeps the assertions for any one cap
	// uncoupled from the noise of the others.
	type dashCounts struct {
		Att, Rec, Items, Plans, Role int
	}
	mk := func(c dashCounts) *DashboardResponse {
		d := &DashboardResponse{
			Attention:      make([]DashboardAttention, c.Att),
			RecentActivity: make([]DashboardActivity, c.Rec),
			ActiveItems:    make([]DashboardActiveItem, c.Items),
			ActivePlans:    make([]DashboardPlan, c.Plans),
			ByRole:         make([]store.RoleBreakdown, c.Role),
		}
		// Populate with identifying values so the cap's slice header
		// retains a defined order — easier to spot index-shifting bugs.
		for i := range d.Attention {
			d.Attention[i] = DashboardAttention{Type: "stalled", ItemRef: fmt.Sprintf("TASK-%d", i)}
		}
		for i := range d.RecentActivity {
			d.RecentActivity[i] = DashboardActivity{Action: "updated", ItemSlug: fmt.Sprintf("item-%d", i)}
		}
		for i := range d.ActiveItems {
			d.ActiveItems[i] = DashboardActiveItem{Slug: fmt.Sprintf("active-%d", i)}
		}
		for i := range d.ActivePlans {
			d.ActivePlans[i] = DashboardPlan{Slug: fmt.Sprintf("plan-%d", i)}
		}
		for i := range d.ByRole {
			d.ByRole[i] = store.RoleBreakdown{}
		}
		return d
	}

	t.Run("under-cap-no-overflow", func(t *testing.T) {
		d := mk(dashCounts{Att: 2, Rec: 3, Items: 1, Plans: 1, Role: 2})
		out := capBootstrapDashboard(d)
		if out == nil {
			t.Fatal("expected non-nil result even when nothing trimmed")
		}
		assertLen(t, "attention", len(out.Attention), 2)
		assertOverflow(t, "attention", out.AttentionOverflowCount, 0)
		assertLen(t, "recent_activity", len(out.RecentActivity), 3)
		assertOverflow(t, "recent_activity", out.RecentActivityOverflowCount, 0)
		assertLen(t, "active_items", len(out.ActiveItems), 1)
		assertOverflow(t, "active_items", out.ActiveItemsOverflowCount, 0)
		assertLen(t, "active_plans", len(out.ActivePlans), 1)
		assertOverflow(t, "active_plans", out.ActivePlansOverflowCount, 0)
		assertLen(t, "by_role", len(out.ByRole), 2)
		assertOverflow(t, "by_role", out.ByRoleOverflowCount, 0)
	})

	t.Run("over-cap-truncates-and-counts-overflow", func(t *testing.T) {
		d := mk(dashCounts{
			Att:   bootstrapAttentionCap + 8,
			Rec:   bootstrapRecentActivityCap + 3,
			Items: bootstrapActiveItemsCap + 4,
			Plans: bootstrapActivePlansCap + 2,
			Role:  bootstrapByRoleCap + 1,
		})
		out := capBootstrapDashboard(d)
		assertLen(t, "attention", len(out.Attention), bootstrapAttentionCap)
		assertOverflow(t, "attention", out.AttentionOverflowCount, 8)
		assertLen(t, "recent_activity", len(out.RecentActivity), bootstrapRecentActivityCap)
		assertOverflow(t, "recent_activity", out.RecentActivityOverflowCount, 3)
		assertLen(t, "active_items", len(out.ActiveItems), bootstrapActiveItemsCap)
		assertOverflow(t, "active_items", out.ActiveItemsOverflowCount, 4)
		assertLen(t, "active_plans", len(out.ActivePlans), bootstrapActivePlansCap)
		assertOverflow(t, "active_plans", out.ActivePlansOverflowCount, 2)
		assertLen(t, "by_role", len(out.ByRole), bootstrapByRoleCap)
		assertOverflow(t, "by_role", out.ByRoleOverflowCount, 1)
	})

	t.Run("source-pointer-unchanged", func(t *testing.T) {
		// Defensive contract: callers downstream of buildDashboardResponse
		// (the dashboard endpoint itself) must see their full-length
		// arrays. The cap mutates a shallow copy.
		d := mk(dashCounts{
			Att:   bootstrapAttentionCap + 5,
			Rec:   bootstrapRecentActivityCap + 5,
			Items: bootstrapActiveItemsCap + 5,
			Plans: bootstrapActivePlansCap + 5,
			Role:  bootstrapByRoleCap + 5,
		})
		want := struct{ att, rec, items, plans, role int }{
			att:   len(d.Attention),
			rec:   len(d.RecentActivity),
			items: len(d.ActiveItems),
			plans: len(d.ActivePlans),
			role:  len(d.ByRole),
		}

		_ = capBootstrapDashboard(d)

		if got := len(d.Attention); got != want.att {
			t.Errorf("source Attention mutated: len = %d, want %d", got, want.att)
		}
		if got := len(d.RecentActivity); got != want.rec {
			t.Errorf("source RecentActivity mutated: len = %d, want %d", got, want.rec)
		}
		if got := len(d.ActiveItems); got != want.items {
			t.Errorf("source ActiveItems mutated: len = %d, want %d", got, want.items)
		}
		if got := len(d.ActivePlans); got != want.plans {
			t.Errorf("source ActivePlans mutated: len = %d, want %d", got, want.plans)
		}
		if got := len(d.ByRole); got != want.role {
			t.Errorf("source ByRole mutated: len = %d, want %d", got, want.role)
		}
	})

	t.Run("exact-cap-no-overflow", func(t *testing.T) {
		// Boundary: len == cap should not flag overflow.
		d := mk(dashCounts{
			Att:   bootstrapAttentionCap,
			Rec:   bootstrapRecentActivityCap,
			Items: bootstrapActiveItemsCap,
			Plans: bootstrapActivePlansCap,
			Role:  bootstrapByRoleCap,
		})
		out := capBootstrapDashboard(d)
		assertOverflow(t, "attention", out.AttentionOverflowCount, 0)
		assertOverflow(t, "recent_activity", out.RecentActivityOverflowCount, 0)
		assertOverflow(t, "active_items", out.ActiveItemsOverflowCount, 0)
		assertOverflow(t, "active_plans", out.ActivePlansOverflowCount, 0)
		assertOverflow(t, "by_role", out.ByRoleOverflowCount, 0)
	})
}

// assertLen and assertOverflow are tiny helpers used by
// TestCapBootstrapDashboard to keep the per-array assertion noise from
// drowning the actual contract being tested. Both call t.Helper() so
// failure lines point at the calling subtest, not at this file.
func assertLen(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s len = %d, want %d", name, got, want)
	}
}

func assertOverflow(t *testing.T, name string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s_overflow_count = %d, want %d", name, got, want)
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
