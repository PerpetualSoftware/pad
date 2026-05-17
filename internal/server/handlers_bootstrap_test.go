package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
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

// TestBootstrapNeedsOnboardingFlag locks PLAN-1496 / TASK-1504's
// contract: the `needs_onboarding` flag is true on a freshly-created
// workspace (no user items yet — only template seeds) and flips to
// false the moment any user/agent-created item exists.
//
// The flag drives the agent skill's bootstrap nudge ("this workspace
// hasn't been set up yet — say /pad onboard"). If this test breaks,
// the nudge fires forever or never.
func TestBootstrapNeedsOnboardingFlag(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Step 1: fresh workspace. createWSForTest hits POST /workspaces
	// with no Template field, so SeedCollectionsFromTemplate runs
	// with templateName="" — backward-compat path that ships ONLY
	// the default system collections and zero seeded items
	// (TASK-1500's onboard auto-seed is gated on a non-empty
	// templateName). Same shape pad-cloud uses for its in-process
	// seed. needs_onboarding should be true here.
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap (initial): %d %s", rr.Code, rr.Body.String())
	}
	var b AgentBootstrap
	parseJSON(t, rr, &b)
	if !b.NeedsOnboarding {
		t.Errorf("fresh workspace: needs_onboarding = false, want true (no user items yet)")
	}

	// Step 2: create a user-side item. Any source != 'template'
	// flips the flag. Item creation via POST /items defaults to
	// source='cli' (matches CLI + Remote MCP attribution).
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]any{
		"title": "First real task",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create user item: %d %s", rr.Code, rr.Body.String())
	}

	// Step 3: re-fetch bootstrap; flag must be false now.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap (after user item): %d %s", rr.Code, rr.Body.String())
	}
	b = AgentBootstrap{}
	parseJSON(t, rr, &b)
	if b.NeedsOnboarding {
		t.Errorf("after creating user item: needs_onboarding = true, want false (nudge should disappear)")
	}
}

// TestBootstrapNeedsOnboardingIgnoresTemplateSeeds is a focused guard:
// even when a templated workspace ships seeded items
// (conventions/playbooks/the onboard playbook itself), they MUST NOT
// flip needs_onboarding to false — only USER-created items count.
// Otherwise startup/scrum/product workspaces would never show the
// nudge and the /pad onboard discovery loop falls apart.
func TestBootstrapNeedsOnboardingIgnoresTemplateSeeds(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{
		"name":     "Startup Needs Onboarding",
		"template": "startup",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", rr.Code, rr.Body.String())
	}
	var b AgentBootstrap
	parseJSON(t, rr, &b)
	if !b.NeedsOnboarding {
		t.Errorf("startup template workspace with only seeded conventions/playbooks: needs_onboarding = false, want true (template seeds don't count)")
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

// TestBootstrapFieldDefMirrorsModelsFieldDef is the drift detector for
// the bootstrapFieldDef parallel struct introduced in TASK-1424.
// bootstrapFieldDef must stay structurally aligned with
// models.FieldDef — same field names, same JSON tags (modulo the
// deliberate `omitempty` on Label and the `json.RawMessage` swap for
// Default). If models.FieldDef adds a new field, this test fails and
// the developer is forced to either mirror it here or explicitly
// decide to drop it from the bootstrap projection.
//
// We compare structural metadata (name + JSON key + base type) rather
// than full Go types so the Default field's `any` → `json.RawMessage`
// swap doesn't trip the check. Detected differences are listed with
// the field name so the failure message points at the gap directly.
func TestBootstrapFieldDefMirrorsModelsFieldDef(t *testing.T) {
	canonical := reflect.TypeOf(models.FieldDef{})
	mirror := reflect.TypeOf(bootstrapFieldDef{})

	if canonical.NumField() != mirror.NumField() {
		t.Fatalf("field count drift: models.FieldDef has %d fields, bootstrapFieldDef has %d — keep them in sync (see godoc on bootstrapFieldDef)",
			canonical.NumField(), mirror.NumField())
	}

	// Map name → JSON tag for the canonical struct.
	canon := make(map[string]string, canonical.NumField())
	for i := 0; i < canonical.NumField(); i++ {
		f := canonical.Field(i)
		canon[f.Name] = f.Tag.Get("json")
	}

	// Allowed JSON-tag delta: the canonical has `json:"label"` (no
	// omitempty); the mirror has `json:"label,omitempty"`. Same key,
	// different presence rule, deliberate.
	tagDeltaAllowed := map[string][2]string{
		"Label": {"label", "label,omitempty"},
	}

	for i := 0; i < mirror.NumField(); i++ {
		mf := mirror.Field(i)
		cTag, ok := canon[mf.Name]
		if !ok {
			t.Errorf("bootstrapFieldDef.%s has no counterpart in models.FieldDef", mf.Name)
			continue
		}
		mTag := mf.Tag.Get("json")
		if mTag == cTag {
			continue
		}
		if allowed, isExpected := tagDeltaAllowed[mf.Name]; isExpected {
			if cTag == allowed[0] && mTag == allowed[1] {
				continue
			}
		}
		t.Errorf("JSON tag drift on %s: models.FieldDef=%q, bootstrapFieldDef=%q (unexpected — update the tagDeltaAllowed table if this divergence is deliberate)",
			mf.Name, cTag, mTag)
	}
}

// TestTrimRedundantSchemaLabels covers the three behaviors of the
// label-trim projection:
//
//  1. Field labels matching TitleCase(key) are dropped.
//  2. Custom labels (those that differ) are preserved.
//  3. Missing/empty labels pass through (no-op).
//
// Plus a malformed-schema edge case where the function must return
// the original bytes verbatim rather than blocking the bootstrap
// response.
func TestTrimRedundantSchemaLabels(t *testing.T) {
	t.Run("drops-redundant-labels", func(t *testing.T) {
		in := []byte(`{"fields":[{"key":"status","label":"Status","type":"select"}]}`)
		out := trimRedundantSchemaLabels(in)

		var parsed map[string][]map[string]json.RawMessage
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("decode trimmed: %v", err)
		}
		if _, ok := parsed["fields"][0]["label"]; ok {
			t.Errorf("redundant `label:\"Status\"` should have been dropped from key=status, got: %s", string(out))
		}
	})

	t.Run("preserves-custom-labels", func(t *testing.T) {
		// label "When" for key "trigger" is NOT TitleCase(key) (which
		// would be "Trigger"), so the custom label MUST be preserved.
		in := []byte(`{"fields":[{"key":"trigger","label":"When","type":"select"}]}`)
		out := trimRedundantSchemaLabels(in)

		var parsed map[string][]map[string]json.RawMessage
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("decode trimmed: %v", err)
		}
		labelRaw, ok := parsed["fields"][0]["label"]
		if !ok {
			t.Fatalf("custom label `When` dropped — would lose information; got: %s", string(out))
		}
		var label string
		_ = json.Unmarshal(labelRaw, &label)
		if label != "When" {
			t.Errorf("custom label mutated: got %q, want %q", label, "When")
		}
	})

	t.Run("multi-word-keys-titlecase-correctly", func(t *testing.T) {
		// snake_case → "Title Case" with spaces. "due_date" → "Due Date".
		in := []byte(`{"fields":[{"key":"due_date","label":"Due Date","type":"date"}]}`)
		out := trimRedundantSchemaLabels(in)

		var parsed map[string][]map[string]json.RawMessage
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("decode trimmed: %v", err)
		}
		if _, ok := parsed["fields"][0]["label"]; ok {
			t.Errorf("redundant `label:\"Due Date\"` should have been dropped from key=due_date, got: %s", string(out))
		}
	})

	t.Run("malformed-schema-returns-raw", func(t *testing.T) {
		// Defensive only — projectBootstrapCollection already gates on
		// json.Valid() upstream, but trimRedundantSchemaLabels should
		// never block a bootstrap response on a parse error.
		bad := []byte(`{"fields":not valid json`)
		out := trimRedundantSchemaLabels(bad)
		if string(out) != string(bad) {
			t.Errorf("malformed schema should pass through verbatim; got %q, want %q", string(out), string(bad))
		}
	})
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
