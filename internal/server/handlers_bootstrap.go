package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

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
	Workspace      AgentBootstrapWorkspace      `json:"workspace"`
	User           AgentBootstrapUser           `json:"user"`
	Collections    []BootstrapCollection        `json:"collections"`
	Conventions    []AgentBootstrapConvention   `json:"conventions"`
	Roles          []models.AgentRole           `json:"roles"`
	Playbooks      []AgentBootstrapPlaybookMeta `json:"playbooks"`
	Dashboard      *DashboardResponse           `json:"dashboard,omitempty"`
	RecentActivity []DashboardActivity          `json:"recent_activity"`
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
	SortOrder       int             `json:"sort_order"`
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
		out.Schema = json.RawMessage(s)
	}
	return out
}

// AgentBootstrapWorkspace is the minimal workspace projection (slug + name
// + id) the agent needs to address the workspace in subsequent calls.
type AgentBootstrapWorkspace struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
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
type AgentBootstrapConvention struct {
	Ref      string `json:"ref"`
	Title    string `json:"title"`
	Slug     string `json:"slug"`
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

// recentActivityWindow caps how far back the bootstrap reaches for the
// recent_activity tail. Bootstrap is a context-load call — anything older
// than this isn't relevant to "what's happening now."
const recentActivityWindow = 24 * time.Hour

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
			ID:   ws.ID,
			Slug: ws.Slug,
			Name: ws.Name,
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
		// Overlay role counts.
		for i := range roles {
			roles[i].ItemCount = roleCounts[roles[i].ID]
		}
	}
	out.Roles = roles

	// Project collections into the slim bootstrap shape now that
	// counts (above) have been recomputed for restricted callers.
	out.Collections = make([]BootstrapCollection, 0, len(collections))
	for _, c := range collections {
		out.Collections = append(out.Collections, projectBootstrapCollection(c))
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
	// context is available. The shape is identical to `GET /dashboard`
	// so the web UI dashboard page can consume bootstrap directly and
	// retire its dedicated fetch.
	if r != nil {
		dash, derr := s.buildDashboardResponse(workspaceID, r)
		if derr == nil {
			out.Dashboard = dash
			if dash != nil {
				out.RecentActivity = capRecentActivity(dash.RecentActivity, recentActivityWindow)
			}
		}
	}
	if out.RecentActivity == nil {
		out.RecentActivity = []DashboardActivity{}
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
			Slug:     it.Slug,
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
			Summary:        playbookSummary(it.Content),
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

// playbookSummary extracts a short prose hint from a playbook body. Picks
// the first non-heading non-empty paragraph and caps at ~240 chars so the
// bootstrap stays compact.
func playbookSummary(body string) string {
	const maxLen = 240
	const ellipsis = "…"
	for _, line := range splitLines(body) {
		trimmed := trimLeadingSpaces(line)
		if trimmed == "" {
			continue
		}
		// Skip markdown headings — they're labels, not summaries.
		if len(trimmed) > 0 && trimmed[0] == '#' {
			continue
		}
		if len(trimmed) > maxLen {
			return trimmed[:maxLen-len(ellipsis)] + ellipsis
		}
		return trimmed
	}
	return ""
}

// splitLines is a small dependency-free helper. We avoid bufio.Scanner
// here because the typical body is small (under 50KB) and allocating a
// scanner per playbook is wasteful at this scale.
func splitLines(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func trimLeadingSpaces(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[i:]
}

// capRecentActivity returns the activity events that fall within the
// bootstrap's recency window. Dashboard's recent_activity may extend
// further back; bootstrap deliberately trims to keep the payload tight.
//
// DashboardActivity.CreatedAt is a pre-formatted RFC3339-shaped string
// (set in handleGetDashboard with `a.CreatedAt.Format("2006-01-02T15:04:05Z")`),
// so we parse it back to time.Time for the comparison. Entries that fail
// to parse are kept defensively — losing them silently because of a format
// glitch would be a worse outcome than carrying a slightly older event.
func capRecentActivity(in []DashboardActivity, window time.Duration) []DashboardActivity {
	if len(in) == 0 {
		return []DashboardActivity{}
	}
	cutoff := time.Now().Add(-window)
	out := make([]DashboardActivity, 0, len(in))
	for _, a := range in {
		t, err := time.Parse("2006-01-02T15:04:05Z", a.CreatedAt)
		if err != nil {
			if t, err = time.Parse(time.RFC3339, a.CreatedAt); err != nil {
				out = append(out, a)
				continue
			}
		}
		if t.Before(cutoff) {
			continue
		}
		out = append(out, a)
	}
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
