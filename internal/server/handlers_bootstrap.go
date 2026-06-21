package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// AgentBootstrap is the single response struct returned by the bootstrap
// endpoint. It consolidates the four /pad context-loading calls (workspace,
// collections, conventions, roles + playbooks) the agent skill used to make
// at every invocation into one round-trip — roughly 200-400ms saved on every
// /pad command.
//
// PLAN-1377 TASK-1379. Same struct is exposed via three MCP surfaces in
// TASK-1380: a `pad://workspace/{ws}/bootstrap` resource, an embedded blob
// in `pad_set_workspace`'s response, and an on-demand `pad_meta.action=bootstrap`
// refresh.
type AgentBootstrap struct {
	Workspace   AgentBootstrapWorkspace      `json:"workspace"`
	User        AgentBootstrapUser           `json:"user"`
	Collections []BootstrapCollection        `json:"collections"`
	Conventions []AgentBootstrapConvention   `json:"conventions"`
	Roles       []BootstrapRole              `json:"roles"`
	Playbooks   []AgentBootstrapPlaybookMeta `json:"playbooks"`
	Dashboard   *BootstrapDashboard          `json:"dashboard,omitempty"`
	// NeedsOnboarding is true when the workspace has ZERO user-created
	// items — i.e. nothing beyond what SeedCollectionsFromTemplate seeded
	// at init time. The agent skill reads this on every /pad invocation
	// and renders a "this workspace hasn't been set up yet — say /pad
	// onboard" nudge while it's true; the moment any user (or agent on
	// their behalf) creates the first real item, the flag flips to false
	// and the nudge disappears. PLAN-1496 / TASK-1504.
	//
	// Computed per-request (not stored). The store predicate is "any
	// item with source != 'template'" — see
	// Store.WorkspaceHasUserCreatedItems for the rationale on why we
	// exclude template seeds rather than enumerating user-side source
	// values.
	NeedsOnboarding bool `json:"needs_onboarding"`
}

// BootstrapCollection is the lightweight collection projection delivered
// in the agent bootstrap response. Distinct from models.Collection: drops
// fields the agent never reads (id, workspace_id, created_at, updated_at,
// deleted_at) and the web-UI `settings` blob (quick-action prompts,
// default views, group-by hints) so the per-invocation payload stays
// tight. The `schema` field is delivered as a nested JSON object rather
// than a JSON-encoded string so the agent sees real `{}`/`[]` structure
// instead of backslash-escaped quotes — roughly 25% byte reduction on
// the schema field alone, more when escape-heavy.
//
// Fields preserved are exactly what the /pad skill consumes:
//   - slug / name / prefix / icon / description — addressing + listing
//   - schema — drives `pad item create/update --field key=value` validation
//   - item_count / active_item_count — surface-area counts in greetings
//   - is_default / is_system — distinguish template seeds from custom collections
//   - sort_order — preserves the workspace's authored ordering
//
// PLAN-1410 / TASK-1412. Pairs with the bootstrapSizeBudget benchmark
// added in TASK-1411 — landing this projection tightens the budget by
// ~25-30% of the collections section.
type BootstrapCollection struct {
	Slug            string          `json:"slug"`
	Name            string          `json:"name"`
	Prefix          string          `json:"prefix"`
	Icon            string          `json:"icon,omitempty"`
	Description     string          `json:"description,omitempty"`
	Schema          json.RawMessage `json:"schema,omitempty"`
	SortOrder       int             `json:"sort_order,omitempty"`
	IsDefault       bool            `json:"is_default"`
	IsSystem        bool            `json:"is_system"`
	ItemCount       int             `json:"item_count"`
	ActiveItemCount int             `json:"active_item_count"`
}

// projectBootstrapCollection converts a models.Collection into the slim
// bootstrap projection. The `schema` field is emitted as a nested JSON
// object when the stored string is valid JSON; an empty or malformed
// schema is omitted (omitempty + nil RawMessage) so the response never
// carries garbage that would break agent-side json.Unmarshal.
//
// json.Valid() is cheap (single byte-stream pass, no allocation) and
// guarantees the wire shape stays parseable even if a future migration
// or buggy write leaves a non-JSON value in the column. Defensive only:
// every collection created via the API today writes valid JSON.
//
// The schema bytes additionally go through trimRedundantSchemaLabels
// to drop fields where `label == TitleCase(key)` — added in TASK-1424
// to cut the schema sub-object's bytes by another ~10% (the labels
// that match the auto-fill rule carry no information the agent can't
// reconstruct).
func projectBootstrapCollection(c models.Collection) BootstrapCollection {
	out := BootstrapCollection{
		Slug:            c.Slug,
		Name:            c.Name,
		Prefix:          c.Prefix,
		Icon:            c.Icon,
		Description:     c.Description,
		SortOrder:       c.SortOrder,
		IsDefault:       c.IsDefault,
		IsSystem:        c.IsSystem,
		ItemCount:       c.ItemCount,
		ActiveItemCount: c.ActiveItemCount,
	}
	if s := strings.TrimSpace(c.Schema); s != "" && json.Valid([]byte(s)) {
		out.Schema = trimRedundantSchemaLabels([]byte(s))
	}
	return out
}

// bootstrapSchema is the bootstrap-side schema shape, parallel to
// models.CollectionSchema. Used only by trimRedundantSchemaLabels to
// round-trip the schema bytes with `omitempty` on FieldDef.Label so
// redundant labels (label == TitleCase(key)) collapse out of the
// wire response. Custom labels — those that differ from the
// auto-fill rule, e.g. label="When" for key="trigger" — are preserved
// unchanged because their value is informationally distinct.
//
// PLAN-1410 / TASK-1424.
type bootstrapSchema struct {
	Fields []bootstrapFieldDef `json:"fields"`
}

// bootstrapFieldDef mirrors models.FieldDef field-for-field with two
// differences: Label is `omitempty` (the whole point), and Default is
// json.RawMessage so we round-trip any default value verbatim without
// re-parsing.
//
// **MUST be kept in sync with models.FieldDef.** Adding a field to
// the canonical struct without mirroring here would silently drop
// the field from the bootstrap schema response.
// TestBootstrapFieldDefMirrorsModelsFieldDef is the drift detector —
// it uses reflection to assert structural parity and fails the build
// if the two diverge.
type bootstrapFieldDef struct {
	Key             string          `json:"key"`
	Label           string          `json:"label,omitempty"`
	Type            string          `json:"type"`
	Options         []string        `json:"options,omitempty"`
	TerminalOptions []string        `json:"terminal_options,omitempty"`
	Default         json.RawMessage `json:"default,omitempty"`
	Required        bool            `json:"required,omitempty"`
	Computed        bool            `json:"computed,omitempty"`
	Collection      string          `json:"collection,omitempty"`
	Suffix          string          `json:"suffix,omitempty"`
	Pattern         string          `json:"pattern,omitempty"`
	UniqueScope     string          `json:"unique_scope,omitempty"`
}

// trimRedundantSchemaLabels parses the schema JSON, drops field
// labels equal to TitleCase(key), and re-marshals. Returns the
// original bytes verbatim on any parse error so a malformed schema
// never blocks the bootstrap response — defensive only, since
// projectBootstrapCollection already gates on json.Valid() upstream.
//
// Field ordering is preserved by struct-based marshalling (Go's
// encoding/json emits fields in struct definition order); we
// deliberately avoid `map[string]any` which would shuffle order.
func trimRedundantSchemaLabels(raw []byte) json.RawMessage {
	var s bootstrapSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		return raw
	}
	for i := range s.Fields {
		if s.Fields[i].Label == titleCaseLabel(s.Fields[i].Key) {
			s.Fields[i].Label = ""
		}
	}
	out, err := json.Marshal(s)
	if err != nil {
		return raw
	}
	return out
}

// titleCaseLabel converts a snake_case key into a Title Case label
// the same way the CLI does ("due_date" → "Due Date"). Duplicated
// from internal/mcp/dispatch_http_routes.go to avoid a server → mcp
// import (mcp already imports server). Keep the two in sync — both
// transformations must produce identical output for a given key.
func titleCaseLabel(key string) string {
	parts := strings.Split(strings.ReplaceAll(key, "_", " "), " ")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// BootstrapRole is the lightweight role projection delivered in the
// agent bootstrap response. Distinct from models.AgentRole: drops
// fields the /pad skill never reads (id, workspace_id, created_at,
// updated_at) and the unused tools column. Same drop-pattern as
// BootstrapCollection — agent addresses roles by slug; UUIDs and
// timestamps are dead weight at context-load time.
//
// Fields preserved are exactly what the /pad skill consumes:
//   - slug — addressing (e.g. `--role <slug>` on item create/update)
//   - name / description / icon — greeting + role-picker rendering
//   - item_count — surface-area count in role-queue greetings
//   - sort_order — preserves the workspace's authored role ordering
//
// PLAN-1410 / TASK-1423. Mirror of TASK-1412's BootstrapCollection
// projection.
type BootstrapRole struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	SortOrder   int    `json:"sort_order"`
	ItemCount   int    `json:"item_count"`
}

// projectBootstrapRole converts a models.AgentRole into the slim
// bootstrap projection. Stateless — the role count recompute for
// restricted callers happens upstream and writes into c.ItemCount
// before this projection runs.
func projectBootstrapRole(r models.AgentRole) BootstrapRole {
	return BootstrapRole{
		Slug:        r.Slug,
		Name:        r.Name,
		Description: r.Description,
		Icon:        r.Icon,
		SortOrder:   r.SortOrder,
		ItemCount:   r.ItemCount,
	}
}

// AgentBootstrapWorkspace is the minimal workspace projection (slug + name
// + id) the agent needs to address the workspace in subsequent calls.
// Description carries the free-text "what are you tracking?" captured at
// workspace creation (PLAN-1847 Phase 3 / TASK-1855) so the onboard playbook
// can start the interview warm instead of asking "what is this project?".
// Omitted when empty — additive to the bootstrap contract, so existing
// clients are unaffected.
type AgentBootstrapWorkspace struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AgentBootstrapUser is the calling user's projection. Email is included
// so agents can sign generated commits or reference the human; ID is
// included so MCP servers can scope per-user data without an extra lookup.
type AgentBootstrapUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// AgentBootstrapConvention is a convention item with its body content so
// agents can read and follow it without a second round-trip. Only
// always-on, active conventions are returned — the curated must-follow
// set. trigger-specific conventions load on demand when their trigger
// fires.
//
// `slug` was dropped in PLAN-1410 / TASK-1413: the agent addresses
// items by `ref` (CONVE-N) — slug was dead weight.
type AgentBootstrapConvention struct {
	Ref      string `json:"ref"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Priority string `json:"priority,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Trigger  string `json:"trigger,omitempty"`
}

// AgentBootstrapPlaybookMeta is the lightweight playbook projection
// returned at bootstrap. Bodies (which can be 5-10KB each) are
// deliberately excluded; the agent loads a body on demand via
// `pad playbook show <slug>` only when a playbook is actually invoked.
// Keeping this metadata-only keeps the bootstrap payload small (~80
// bytes per entry) so a workspace with dozens of playbooks doesn't blow
// out the agent's context budget on a /pad greeting.
type AgentBootstrapPlaybookMeta struct {
	Ref            string `json:"ref"`
	Title          string `json:"title"`
	Slug           string `json:"slug"`
	InvocationSlug string `json:"invocation_slug,omitempty"`
	Trigger        string `json:"trigger,omitempty"`
	Scope          string `json:"scope,omitempty"`
	Status         string `json:"status,omitempty"`
	// HasArguments is true when the playbook declares an arguments spec
	// in its fields. The full spec is delivered on demand at invocation.
	HasArguments bool `json:"has_arguments"`
	// Summary is a short prose hint about what the playbook does, taken
	// from the first non-heading non-empty paragraph of the body. Capped
	// at ~240 chars so the bootstrap stays small.
	Summary string `json:"summary,omitempty"`
}

// BootstrapDashboard is the bootstrap-side dashboard projection. It
// embeds *DashboardResponse so the wire shape stays compatible with the
// `GET /dashboard` endpoint (same field names, same nesting), then adds
// five overflow counts (one per capped sub-array) that report how many
// entries were trimmed from the bootstrap's capped views.
//
// Why a wrapper rather than mutating DashboardResponse: the cap is
// bootstrap-only — `pad project dashboard` and the web UI's dashboard
// page consume the FULL set, unchanged. PLAN-1410 / TASK-1413.
type BootstrapDashboard struct {
	*DashboardResponse
	// AttentionOverflowCount is len(original attention) - cap, or
	// omitted when nothing was trimmed. The agent reads this to decide
	// whether to suggest pulling the full set via `pad project dashboard`.
	AttentionOverflowCount int `json:"attention_overflow_count,omitempty"`
	// RecentActivityOverflowCount mirrors AttentionOverflowCount for the
	// recent_activity tail.
	RecentActivityOverflowCount int `json:"recent_activity_overflow_count,omitempty"`
	// ActiveItemsOverflowCount, ActivePlansOverflowCount, and
	// ByRoleOverflowCount cap the three other dashboard sub-arrays
	// that grow with workspace state. Same semantics as the
	// Attention/RecentActivity counts above: omitted when zero,
	// populated with `len(original) - cap` when truncation kicked in.
	// PLAN-1410 / TASK-1422 (absorbs IDEA-1421).
	//
	// `suggested_next` was deliberately NOT added to this set:
	// buildDashboardResponse already truncates SuggestedNext to 3
	// upstream (see "Take top 3" in handlers_dashboard.go), so a
	// bootstrap-side cap of 5 would be unreachable dead code. If the
	// upstream cap is ever raised or removed, that's the moment to
	// add a suggested_next_overflow_count here.
	ActiveItemsOverflowCount int `json:"active_items_overflow_count,omitempty"`
	ActivePlansOverflowCount int `json:"active_plans_overflow_count,omitempty"`
	ByRoleOverflowCount      int `json:"by_role_overflow_count,omitempty"`
}

// Bootstrap caps clamp the per-array sizes in the bootstrap dashboard
// projection. 5 is the practical surfacing depth for an agent greeting
// or status pass — anything beyond is too much for a single response
// to render conversationally; the agent should pivot to the full
// `pad project dashboard` query when an overflow count signals more
// work to consider. PLAN-1410. The first two land in TASK-1413; the
// remaining three (active_items / active_plans / by_role) are TASK-1422
// (IDEA-1421 absorbed). suggested_next is excluded — upstream cap of 3.
const (
	bootstrapAttentionCap      = 5
	bootstrapRecentActivityCap = 5
	bootstrapActiveItemsCap    = 5
	bootstrapActivePlansCap    = 5
	bootstrapByRoleCap         = 5
)

// isCollectionSlugVisible reports whether the named collection survived
// the visibility filter. Used by the bootstrap path to gate
// convention/playbook queries on whether the caller can see those
// collections at all. The slice we're checking is already-filtered, so
// presence implies visibility.
func isCollectionSlugVisible(filtered []models.Collection, slug string) bool {
	for _, c := range filtered {
		if c.Slug == slug {
			return true
		}
	}
	return false
}

// BuildAgentBootstrap assembles the bootstrap blob from store queries.
// This is the single canonical code path; the HTTP handler, the MCP
// resource handler, and the MCP `pad_set_workspace` embed all call this.
//
// r is the live request — used for the dashboard sub-build AND to
// resolve the calling principal's collection visibility / guest grant
// filter. Pass nil only when no request context is available (e.g. a
// future MCP in-process dispatcher synthesizing its own ACL context);
// in that case the bootstrap returns the full workspace view, which is
// safe ONLY for callers that have already verified full-member access
// out-of-band. Production HTTP/MCP paths MUST pass the live request.
func (s *Server) BuildAgentBootstrap(workspaceID string, user *models.User, r *http.Request) (*AgentBootstrap, error) {
	ws, err := s.store.GetWorkspaceByID(workspaceID)
	if err != nil {
		return nil, err
	}

	out := &AgentBootstrap{
		Workspace: AgentBootstrapWorkspace{
			ID:          ws.ID,
			Slug:        ws.Slug,
			Name:        ws.Name,
			Description: ws.Description,
		},
	}
	if user != nil {
		out.User = AgentBootstrapUser{
			ID:    user.ID,
			Name:  user.Name,
			Email: user.Email,
		}
	}

	// Resolve visibility once so collections/conventions/playbooks/roles
	// all project the same authorized view. nil visibleIDs means "no
	// restriction" — a full workspace member (or a nil-r caller that
	// has already verified access out-of-band). For guests with
	// item-level grants, we also need ItemIDs filtering so a grant to
	// one specific playbook doesn't leak the whole collection.
	var visibleIDs []string
	var grantedItemIDs []string
	var fullCollIDs []string
	if r != nil {
		visibleIDs, err = s.visibleCollectionIDs(r, workspaceID)
		if err != nil {
			return nil, err
		}
		fullCollIDs, grantedItemIDs, err = s.guestResourceFilter(r, workspaceID)
		if err != nil {
			return nil, err
		}
	}

	// Collections — load and apply visibility filtering. We hold these
	// in their full models.Collection shape through the role/count
	// recompute below (which keys lookups by Collection.ID), then
	// project to BootstrapCollection at the end of this section. The
	// projection drops id/workspace_id/timestamps/settings and parses
	// the schema string into a nested object — see BootstrapCollection
	// godoc + PLAN-1410 / TASK-1412.
	collections, err := s.store.ListCollections(workspaceID)
	if err != nil {
		return nil, err
	}
	if visibleIDs != nil {
		filtered := make([]models.Collection, 0, len(collections))
		for _, c := range collections {
			if isCollectionVisible(c.ID, visibleIDs) {
				filtered = append(filtered, c)
			}
		}
		collections = filtered
	}

	// Build the (collectionIDs, itemIDs) tuple the sub-queries should
	// project through. When the caller has item-level grants, switch to
	// the full-coll vs granted-item filter — same shape handleListItems
	// uses. Without grants, fall back to collection-level visibility.
	subCollIDs := visibleIDs
	var subItemIDs []string
	if len(grantedItemIDs) > 0 {
		subCollIDs = fullCollIDs
		subItemIDs = grantedItemIDs
	}

	// Conventions — only the always-on, active set, restricted by the
	// caller's authorized view. A guest with a grant to one specific
	// convention item gets only that item, not the whole always-on set.
	conventionsCollVisible := visibleIDs == nil || isCollectionSlugVisible(collections, "conventions")
	if conventionsCollVisible {
		convs, cerr := s.collectAlwaysOnConventions(workspaceID, subCollIDs, subItemIDs)
		if cerr != nil {
			return nil, cerr
		}
		out.Conventions = convs
	} else {
		out.Conventions = []AgentBootstrapConvention{}
	}

	// Agent roles — workspace-scoped, not collection-bound. Item counts
	// MUST be recomputed below for restricted callers from the same
	// visible item set used for collection counts.
	roles, err := s.store.ListAgentRoles(workspaceID)
	if err != nil {
		return nil, err
	}
	if roles == nil {
		roles = []models.AgentRole{}
	}

	// For restricted callers, compute the visible item set ONCE and use
	// it to (a) recompute role counts and (b) recompute collection
	// item_count, both of which are otherwise computed across the whole
	// workspace by their respective ListX queries and would leak
	// hidden activity to a guest. Full members (visibleIDs == nil)
	// skip the recompute — the store-side counts are already correct
	// for them.
	if visibleIDs != nil {
		visibleItems, vierr := s.store.ListItems(workspaceID, models.ItemListParams{
			CollectionIDs: subCollIDs,
			ItemIDs:       subItemIDs,
		})
		if vierr != nil {
			return nil, vierr
		}
		roleCounts := make(map[string]int)
		collItemCounts := make(map[string]int)
		for _, item := range visibleItems {
			if item.AgentRoleID != nil && *item.AgentRoleID != "" {
				roleCounts[*item.AgentRoleID]++
			}
			collItemCounts[item.CollectionID]++
		}
		// Rewrite collection counts from the visible set.
		// active_item_count needs each collection's done-rules to be
		// accurate; recomputing it correctly is expensive (see
		// dashboard's buildDoneContextMap) and the bootstrap consumers
		// don't depend on it. Set it equal to item_count so restricted
		// callers see a self-consistent number rather than a leaked
		// full-workspace value. Full members keep the store-side
		// active_item_count untouched.
		//
		// We mutate the local `collections` slice (models.Collection
		// shape) here, before the bootstrap projection below — keyed
		// by Collection.ID, which the projection drops.
		for i := range collections {
			c := &collections[i]
			c.ItemCount = collItemCounts[c.ID]
			c.ActiveItemCount = collItemCounts[c.ID]
		}
		// Overlay role counts. Same shape as collections above — mutate
		// the local `roles` slice (models.AgentRole) here before the
		// bootstrap projection below, keyed by AgentRole.ID which the
		// projection drops.
		for i := range roles {
			roles[i].ItemCount = roleCounts[roles[i].ID]
		}
	}

	// Project collections AND roles into their slim bootstrap shapes
	// now that counts (above) have been recomputed for restricted
	// callers. Both projections drop UUIDs / workspace IDs / timestamps;
	// the agent addresses them by slug.
	out.Collections = make([]BootstrapCollection, 0, len(collections))
	for _, c := range collections {
		out.Collections = append(out.Collections, projectBootstrapCollection(c))
	}
	out.Roles = make([]BootstrapRole, 0, len(roles))
	for _, r := range roles {
		out.Roles = append(out.Roles, projectBootstrapRole(r))
	}

	// Playbooks (metadata only) — restricted to the caller's authorized
	// view. A guest granted one specific playbook item sees that one,
	// not the whole collection.
	playbooksCollVisible := visibleIDs == nil || isCollectionSlugVisible(collections, "playbooks")
	if playbooksCollVisible {
		playbooks, perr := s.collectPlaybookMetadata(workspaceID, subCollIDs, subItemIDs)
		if perr != nil {
			return nil, perr
		}
		out.Playbooks = playbooks
	} else {
		out.Playbooks = []AgentBootstrapPlaybookMeta{}
	}

	// Dashboard — recreate via the existing handler logic if a request
	// context is available, then wrap in BootstrapDashboard so the
	// bootstrap-side caps on `attention`, `recent_activity`,
	// `active_items`, `active_plans`, and `by_role` (each with its
	// `*_overflow_count` companion) don't leak into the
	// `GET /dashboard` contract.
	if r != nil {
		dash, derr := s.buildDashboardResponse(workspaceID, r)
		if derr == nil && dash != nil {
			out.Dashboard = capBootstrapDashboard(dash)
		}
	}

	// NeedsOnboarding: workspace-level signal (no per-caller visibility
	// filtering — see Store.WorkspaceHasUserCreatedItems comment for
	// the rationale). On a query error we fall back to false, which
	// matches the safe default of "don't nag the agent." Worst case is
	// a missed nudge; the opposite (nagging an already-onboarded
	// workspace) is worse UX. PLAN-1496 / TASK-1504.
	hasUserItems, hErr := s.store.WorkspaceHasUserCreatedItems(workspaceID)
	if hErr == nil {
		out.NeedsOnboarding = !hasUserItems
	}

	return out, nil
}

// collectAlwaysOnConventions returns the active, always-on conventions for
// a workspace, projected into the bootstrap-friendly shape. Sorted by
// priority (must > should > nice-to-have) then by ref for stable order.
//
// collIDs / itemIDs scope the underlying ListItems call: nil collIDs
// means "no restriction" (full member); non-nil collIDs + non-nil
// itemIDs is the guest-with-item-grants shape from guestResourceFilter.
// A guest granted access to a single convention only sees that one.
func (s *Server) collectAlwaysOnConventions(workspaceID string, collIDs []string, itemIDs []string) ([]AgentBootstrapConvention, error) {
	items, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionSlug: "conventions",
		CollectionIDs:  collIDs,
		ItemIDs:        itemIDs,
		Fields: map[string]string{
			"status":  "active",
			"trigger": "always",
		},
	})
	if err != nil {
		return nil, err
	}
	out := make([]AgentBootstrapConvention, 0, len(items))
	for _, it := range items {
		fields := map[string]any{}
		_ = json.Unmarshal([]byte(it.Fields), &fields)
		strField := func(k string) string {
			if v, ok := fields[k].(string); ok {
				return v
			}
			return ""
		}
		out = append(out, AgentBootstrapConvention{
			Ref:      it.Ref,
			Title:    it.Title,
			Content:  it.Content,
			Priority: strField("priority"),
			Scope:    strField("scope"),
			Trigger:  strField("trigger"),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi := conventionPriorityRank(out[i].Priority)
		pj := conventionPriorityRank(out[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return out[i].Ref < out[j].Ref
	})
	return out, nil
}

// conventionPriorityRank ranks convention priority strings
// (must > should > nice-to-have). Lower rank = higher priority. Unknown
// values rank last so untyped data doesn't dominate the head of the list.
// Distinct from the task `priorityRank` in handlers_dashboard.go because
// conventions and tasks have disjoint priority vocabularies.
func conventionPriorityRank(p string) int {
	switch p {
	case "must":
		return 0
	case "should":
		return 1
	case "nice-to-have":
		return 2
	default:
		return 3
	}
}

// collectPlaybookMetadata returns every playbook in the workspace projected
// down to the metadata shape. Bodies are NOT included.
//
// collIDs / itemIDs scope the underlying ListItems call: nil collIDs
// means "no restriction" (full member); non-nil collIDs + non-nil
// itemIDs is the guest-with-item-grants shape from guestResourceFilter.
// A guest granted access to a single playbook only sees that one.
func (s *Server) collectPlaybookMetadata(workspaceID string, collIDs []string, itemIDs []string) ([]AgentBootstrapPlaybookMeta, error) {
	items, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionSlug: "playbooks",
		CollectionIDs:  collIDs,
		ItemIDs:        itemIDs,
	})
	if err != nil {
		return nil, err
	}
	out := make([]AgentBootstrapPlaybookMeta, 0, len(items))
	for _, it := range items {
		fields := map[string]any{}
		_ = json.Unmarshal([]byte(it.Fields), &fields)
		strField := func(k string) string {
			if v, ok := fields[k].(string); ok {
				return v
			}
			return ""
		}
		args, hasArgs := fields["arguments"]
		// Treat empty arrays / objects as "no arguments declared". A
		// playbook with an empty arguments array is functionally
		// identical to one that omits the field entirely.
		if hasArgs {
			switch v := args.(type) {
			case []any:
				hasArgs = len(v) > 0
			case map[string]any:
				hasArgs = len(v) > 0
			case nil:
				hasArgs = false
			}
		}
		out = append(out, AgentBootstrapPlaybookMeta{
			Ref:            it.Ref,
			Title:          it.Title,
			Slug:           it.Slug,
			InvocationSlug: strField("invocation_slug"),
			Trigger:        strField("trigger"),
			Scope:          strField("scope"),
			Status:         strField("status"),
			HasArguments:   hasArgs,
			Summary:        collections.PlaybookSummary(it.Content),
		})
	}
	// Stable order: invocation_slug-bearing first (the user-facing,
	// directly-callable set), then alphabetic by title within each group.
	sort.SliceStable(out, func(i, j int) bool {
		ai := out[i].InvocationSlug != ""
		aj := out[j].InvocationSlug != ""
		if ai != aj {
			return ai
		}
		return out[i].Title < out[j].Title
	})
	return out, nil
}

// capBootstrapDashboard wraps a DashboardResponse with the bootstrap's
// per-section caps. The underlying *DashboardResponse is shallow-copied
// before the slice headers are reslized so the caller's pointer (used by
// the dashboard endpoint elsewhere) sees its original full-length
// arrays unchanged. The slice backing arrays are shared — we only
// trim the view, no allocation needed for the truncated portion.
//
// Returns a non-nil *BootstrapDashboard even when all caps are
// untriggered, so the agent always sees a consistent shape.
func capBootstrapDashboard(d *DashboardResponse) *BootstrapDashboard {
	copied := *d
	out := &BootstrapDashboard{DashboardResponse: &copied}
	if n := len(copied.Attention) - bootstrapAttentionCap; n > 0 {
		copied.Attention = copied.Attention[:bootstrapAttentionCap]
		out.AttentionOverflowCount = n
	}
	if n := len(copied.RecentActivity) - bootstrapRecentActivityCap; n > 0 {
		copied.RecentActivity = copied.RecentActivity[:bootstrapRecentActivityCap]
		out.RecentActivityOverflowCount = n
	}
	if n := len(copied.ActiveItems) - bootstrapActiveItemsCap; n > 0 {
		copied.ActiveItems = copied.ActiveItems[:bootstrapActiveItemsCap]
		out.ActiveItemsOverflowCount = n
	}
	if n := len(copied.ActivePlans) - bootstrapActivePlansCap; n > 0 {
		copied.ActivePlans = copied.ActivePlans[:bootstrapActivePlansCap]
		out.ActivePlansOverflowCount = n
	}
	if n := len(copied.ByRole) - bootstrapByRoleCap; n > 0 {
		copied.ByRole = copied.ByRole[:bootstrapByRoleCap]
		out.ByRoleOverflowCount = n
	}
	// SuggestedNext intentionally NOT capped here — see godoc on
	// BootstrapDashboard.
	return out
}

// handleGetBootstrap is the HTTP handler for `GET
// /api/v1/workspaces/{ws}/agent/bootstrap`. It returns the consolidated
// AgentBootstrap blob in one round-trip so the /pad skill can replace its
// four context-loading CLI calls with one.
func (s *Server) handleGetBootstrap(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	user := currentUser(r)
	bootstrap, err := s.BuildAgentBootstrap(workspaceID, user, r)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bootstrap)
}
