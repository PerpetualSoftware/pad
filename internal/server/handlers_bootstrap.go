package server

import (
	"encoding/json"
	"net/http"
	"sort"
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
	Collections    []models.Collection          `json:"collections"`
	Conventions    []AgentBootstrapConvention   `json:"conventions"`
	Roles          []models.AgentRole           `json:"roles"`
	Playbooks      []AgentBootstrapPlaybookMeta `json:"playbooks"`
	Dashboard      *DashboardResponse           `json:"dashboard,omitempty"`
	RecentActivity []DashboardActivity          `json:"recent_activity"`
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

	// Resolve visibility once so collections/conventions/playbooks all
	// project the same authorized view. nil visibleIDs means "no
	// restriction" — a full workspace member (or a nil-r caller that
	// has already verified access out-of-band).
	var visibleIDs []string
	if r != nil {
		visibleIDs, err = s.visibleCollectionIDs(r, workspaceID)
		if err != nil {
			return nil, err
		}
	}

	// Collections — keep the same shape ListCollections returns, filtered
	// by visibility so a guest only sees collections they're permitted
	// into. Mirrors handleListCollections.
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
	out.Collections = collections

	// Conventions — only the always-on, active set, restricted to the
	// caller's visible conventions collection. trigger-specific
	// conventions load on demand when their trigger fires.
	conventionsCollVisible := visibleIDs == nil || isCollectionSlugVisible(out.Collections, "conventions")
	if conventionsCollVisible {
		convs, cerr := s.collectAlwaysOnConventions(workspaceID)
		if cerr != nil {
			return nil, cerr
		}
		out.Conventions = convs
	} else {
		out.Conventions = []AgentBootstrapConvention{}
	}

	// Agent roles — workspace-scoped, not collection-bound. Visible to
	// any principal admitted into the workspace.
	roles, err := s.store.ListAgentRoles(workspaceID)
	if err != nil {
		return nil, err
	}
	if roles == nil {
		roles = []models.AgentRole{}
	}
	out.Roles = roles

	// Playbooks (metadata only) — restricted to callers who can see the
	// playbooks collection. Like conventions, this is a collection-level
	// gate: if the caller can't see the playbooks collection at all,
	// they get an empty list.
	playbooksCollVisible := visibleIDs == nil || isCollectionSlugVisible(out.Collections, "playbooks")
	if playbooksCollVisible {
		playbooks, perr := s.collectPlaybookMetadata(workspaceID)
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
func (s *Server) collectAlwaysOnConventions(workspaceID string) ([]AgentBootstrapConvention, error) {
	items, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionSlug: "conventions",
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
func (s *Server) collectPlaybookMetadata(workspaceID string) ([]AgentBootstrapPlaybookMeta, error) {
	items, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionSlug: "playbooks",
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
