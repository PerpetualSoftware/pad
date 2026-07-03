package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// This file implements the browser/REST surface for the "project
// intelligence" reads (PLAN-1888 / TASK-1894): next, standup, changelog.
//
// KEEP IN SYNC — these three handlers are the THIRD reproduction of this
// reshaping contract, alongside:
//   - cmd/pad/main.go: nextCmd / standupCmd / changelogCmd (the canonical
//     CLI implementations `pad project next|standup|changelog --format json`)
//   - internal/mcp/dispatch_http_slice4.go: dispatchProjectNext /
//     dispatchProjectStandup / dispatchProjectChangelog (the MCP HTTP
//     transport's reproduction of the same contract)
//
// All three must agree on defaults, per-status best-effort semantics, and
// output shape. A follow-up task will consolidate slice4 onto these REST
// endpoints (route-table + delete the custom dispatchers) — until then,
// changes here need a matching change (or an explicit noted divergence) in
// both siblings above.
//
// Known asymmetry (TASK-1894 codex round 1): handleGetProjectStandup scopes
// its own completed/in_progress ListItems calls through the bearer-aware
// projectIntelVisibility (see below), but its dashboard-derived sections
// (blockers, suggested_next) come from buildDashboardResponse, which is
// NOT bearer-aware — same gap as handleGetProjectNext, which deliberately
// stays on buildDashboardResponse's ungated visibility so it keeps exact
// parity with dashboard.suggested_next (gating next but not dashboard would
// break that parity for bearer admins and diverge next from both siblings).
// Net effect: a bearer-authed platform admin who is only a restricted member
// of the workspace gets correctly-scoped completed/in_progress from standup,
// but ungated blockers/suggested_next — until BUG-1917 (the
// visibleCollectionIDs bearer-gate gap) is fixed for buildDashboardResponse
// itself, at which point this asymmetry resolves.

// parseDaysParam reads a "days" query param with a fallback default. Mirrors
// the CLI's `n > 0` gate (standupCmd/changelogCmd flag defaults) and the MCP
// HTTP transport's `numericInput(...) && n > 0` gate in dispatch_http_slice4.go:
// absent, non-numeric, zero, or negative all silently fall back to the
// default rather than erroring — this also matches this codebase's existing
// REST convention for lenient numeric query params (handleGetWorkspaceGraph's
// `depth`, GetReport's `window`), so days is not a divergence from either
// sibling or the surrounding REST style.
func parseDaysParam(r *http.Request, def int) int {
	raw := strings.TrimSpace(r.URL.Query().Get("days"))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// itemMatchesParentFilter mirrors the CLI's parent-filter matching
// (cmd/pad/main.go's changelogCmd) and dispatch_http_slice4.go's
// itemMatchesParent: case-insensitive comparison against the item's parent
// link id, parent ref, or parent title. Lets an agent/user pass a UUID, an
// issue ref like "PLAN-3", or the human-readable title.
func itemMatchesParentFilter(item models.Item, parent string) bool {
	for _, v := range []string{item.ParentLinkID, item.ParentRef, item.ParentTitle} {
		if v != "" && strings.EqualFold(v, parent) {
			return true
		}
	}
	return false
}

// bearerAwareVisibleCollectionIDs mirrors visibleCollectionIDs (server.go)
// but closes the gap reportVisibleCollections (handlers_reports.go) already
// closes for the report endpoint (the BUG-1616/1617 pattern — see its doc
// comment and handlers_admin_bearer_gate_test.go): a platform admin
// authenticated via a bearer token (PAT/CLI/OAuth) who is only a restricted
// member of THIS workspace must be scoped to their real membership, not
// granted the unrestricted admin view a cookie-session admin gets.
// visibleCollectionIDs itself — and everything still built directly on it
// (buildDashboardResponse, handleListItems, handleGetWorkspaceGraph) — does
// NOT have this gate; that's BUG-1917, tracked separately from this fix.
func (s *Server) bearerAwareVisibleCollectionIDs(r *http.Request, workspaceID string) ([]string, error) {
	user := currentUser(r)
	if user == nil || (user.Role == "admin" && !isBearerAuth(r)) {
		return nil, nil // unrestricted, same as visibleCollectionIDs' nil-user/admin branch
	}
	return s.store.VisibleCollectionIDs(workspaceID, user.ID)
}

// projectIntelVisibility computes the (collectionIDs, itemIDs)
// visibility-scoping pair for the item-list calls the standup/changelog
// handlers make directly (outside of buildDashboardResponse), using
// bearerAwareVisibleCollectionIDs rather than visibleCollectionIDs.
//
// This does NOT delegate to reportVisibleCollections despite the shared
// admin-bypass gate: reportVisibleCollections returns (scopeToVisible bool,
// ids []string) and deliberately drops item-level grant granularity, because
// aggregate report counts have no item-level filtering (a collection is
// either fully in-scope or fully excluded — see its doc comment). standup and
// changelog list actual items, so they need the same item-level grant
// filtering handleListItems applies (ItemListParams.ItemIDs) — hence the
// (collIDs, itemIDs) shape and the guestResourceFilter call below, matching
// handleListItems' pattern exactly except for the visibility call itself.
//
// visibleIDs == nil covers BOTH "unrestricted" (nil/cookie-admin user) and
// "all-access member" (store.VisibleCollectionIDs returns nil) — both mean
// "no collection filtering", identically to visibleCollectionIDs' contract,
// so no separate branch is needed for the all-access-member case.
func (s *Server) projectIntelVisibility(r *http.Request, workspaceID string) (collIDs, itemIDs []string, err error) {
	visibleIDs, err := s.bearerAwareVisibleCollectionIDs(r, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		return nil, nil, err
	}
	collIDs = visibleIDs
	if len(grantedItemIDs) > 0 {
		collIDs = fullCollIDs
		itemIDs = grantedItemIDs
	}
	return collIDs, itemIDs, nil
}

// listTerminalItemsSince fetches items in each of models.DefaultTerminalStatuses
// (one ListItems call per status — mirrors the CLI/slice4 loop rather than
// OR-ing statuses into a single query), keeping only items updated after
// cutoff, up to limit items considered per status. A store error for one
// status is swallowed and the loop continues — matches the CLI's and
// slice4's best-effort semantics: a transient failure on one status must not
// blank out the whole report.
func (s *Server) listTerminalItemsSince(
	workspaceID string, collIDs, itemIDs []string, cutoff time.Time, limit int,
) []models.Item {
	var out []models.Item
	for _, status := range models.DefaultTerminalStatuses {
		items, err := s.store.ListItems(workspaceID, models.ItemListParams{
			CollectionIDs: collIDs,
			ItemIDs:       itemIDs,
			Fields:        map[string]string{"status": status},
			Sort:          "updated_at:desc",
			Limit:         limit,
		})
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.UpdatedAt.After(cutoff) {
				out = append(out, item)
			}
		}
	}
	return out
}

// --- GET /workspaces/{slug}/next ---

// handleGetProjectNext returns the dashboard's suggested_next array — the
// same data `pad project next --format json` prints (cmd/pad/main.go's
// nextCmd, post BUG-987 bug 6: ONLY the suggested_next array, not the whole
// dashboard) and dispatchProjectNext (dispatch_http_slice4.go) returns over
// the MCP HTTP transport. Returned as a bare JSON array (matching the CLI's
// `cli.PrintJSON(dash.SuggestedNext)` and the handleListItems bare-array
// convention), never null (buildDashboardResponse initializes SuggestedNext
// to []DashboardSuggestion{}).
func (s *Server) handleGetProjectNext(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	resp, err := s.buildDashboardResponse(workspaceID, r)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp.SuggestedNext)
}

// --- GET /workspaces/{slug}/standup ---

// StandupItem is one row in a StandupResponse list. Field-for-field mirror
// of cmd/pad/main.go's standupCmd `standupItem` JSON type and
// dispatch_http_slice4.go's dispatchProjectStandup `standupItem` type.
type StandupItem struct {
	Ref      string `json:"ref"`
	Title    string `json:"title"`
	Status   string `json:"status,omitempty"`
	Priority string `json:"priority,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// StandupResponse mirrors cmd/pad/main.go's standupCmd `standupJSON` type
// and dispatch_http_slice4.go's dispatchProjectStandup payload map exactly.
type StandupResponse struct {
	Date          string        `json:"date"`
	Days          int           `json:"days"`
	Completed     []StandupItem `json:"completed"`
	InProgress    []StandupItem `json:"in_progress"`
	Blockers      []StandupItem `json:"blockers"`
	SuggestedNext []StandupItem `json:"suggested_next"`
}

// handleGetProjectStandup serves GET /workspaces/{slug}/standup?days=N.
//
//	days (optional, default 1) — lookback window for "completed" items.
//	See parseDaysParam for the lenient-default semantics.
//
// Mirrors `pad project standup --format json` (cmd/pad/main.go standupCmd)
// and dispatchProjectStandup (dispatch_http_slice4.go) exactly: dashboard
// for active items / blockers / suggested-next, plus a per-terminal-status
// ListItems loop (limit 20, best-effort) for "completed", plus an unbounded
// in-progress ListItems call.
func (s *Server) handleGetProjectStandup(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	days := parseDaysParam(r, 1)
	cutoff := time.Now().AddDate(0, 0, -days)

	dash, err := s.buildDashboardResponse(workspaceID, r)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	collIDs, itemIDs, err := s.projectIntelVisibility(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	completed := s.listTerminalItemsSince(workspaceID, collIDs, itemIDs, cutoff, 20)

	// In-progress items: unbounded, best-effort (a store error yields an
	// empty list rather than failing the whole standup — matches the CLI's
	// `inProgressItems = nil` fallback and slice4's identical tolerance).
	inProgress, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionIDs: collIDs,
		ItemIDs:       itemIDs,
		Fields:        map[string]string{"status": "in-progress"},
		Sort:          "updated_at:desc",
	})
	if err != nil {
		inProgress = nil
	}

	resp := StandupResponse{
		Date:          time.Now().Format("2006-01-02"),
		Days:          days,
		Completed:     make([]StandupItem, 0, len(completed)),
		InProgress:    make([]StandupItem, 0, len(inProgress)),
		Blockers:      make([]StandupItem, 0, len(dash.Attention)),
		SuggestedNext: make([]StandupItem, 0, len(dash.SuggestedNext)),
	}
	for _, item := range completed {
		resp.Completed = append(resp.Completed, StandupItem{
			Ref:    item.Ref,
			Title:  item.Title,
			Status: extractFieldValue(item.Fields, "status"),
		})
	}
	for _, item := range inProgress {
		resp.InProgress = append(resp.InProgress, StandupItem{
			Ref:      item.Ref,
			Title:    item.Title,
			Priority: extractFieldValue(item.Fields, "priority"),
		})
	}
	for _, a := range dash.Attention {
		resp.Blockers = append(resp.Blockers, StandupItem{
			Ref:    a.ItemRef,
			Title:  a.ItemTitle,
			Reason: a.Reason,
		})
	}
	for _, sug := range dash.SuggestedNext {
		resp.SuggestedNext = append(resp.SuggestedNext, StandupItem{
			Ref:    sug.ItemRef,
			Title:  sug.ItemTitle,
			Reason: sug.Reason,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// --- GET /workspaces/{slug}/changelog ---

// ChangelogItem is one row in a ChangelogGroup. Field-for-field mirror of
// cmd/pad/main.go's changelogCmd `changelogItem` JSON type and
// dispatch_http_slice4.go's dispatchProjectChangelog `changelogItem` type.
type ChangelogItem struct {
	Ref    string `json:"ref"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// ChangelogGroup is one collection's bucket of completed items within the
// changelog window. Mirrors changelogCmd's `changelogGroup` /
// dispatchProjectChangelog's `changelogGroup`.
type ChangelogGroup struct {
	Collection string          `json:"collection"`
	Icon       string          `json:"icon,omitempty"`
	Count      int             `json:"count"`
	Items      []ChangelogItem `json:"items"`
}

// ChangelogResponse mirrors changelogCmd's `changelogJSON` type and
// dispatchProjectChangelog's payload map exactly.
type ChangelogResponse struct {
	Period string           `json:"period"`
	Since  string           `json:"since"`
	Total  int              `json:"total"`
	Groups []ChangelogGroup `json:"groups"`
}

// handleGetProjectChangelog serves:
//
//	GET /workspaces/{slug}/changelog?days=N&since=YYYY-MM-DD&parent=REF
//
//	days   (optional, default 7) — lookback window. See parseDaysParam.
//	since  (optional, YYYY-MM-DD) — takes precedence over days when both are
//	       given (silent since-wins, matching the CLI's if/else and
//	       dispatchProjectChangelog exactly — all three surfaces already
//	       agree on this, REST doesn't get to be the odd one out). Malformed
//	       since → 400 bad_request (matches the CLI's hard error and
//	       dispatchProjectChangelog's validationFailedResult).
//	parent (optional, ref/slug/UUID/title) — scope to items whose parent
//	       matches, via itemMatchesParentFilter.
//
// Mirrors `pad project changelog --format json` (cmd/pad/main.go
// changelogCmd) and dispatchProjectChangelog (dispatch_http_slice4.go)
// exactly: a per-terminal-status ListItems loop (limit 100, best-effort),
// cutoff + parent filtering, grouped by collection preserving first-seen
// order.
func (s *Server) handleGetProjectChangelog(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	days := parseDaysParam(r, 7)
	since := strings.TrimSpace(r.URL.Query().Get("since"))
	parent := strings.TrimSpace(r.URL.Query().Get("parent"))

	var cutoff time.Time
	if since != "" {
		parsed, err := time.Parse("2006-01-02", since)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"invalid 'since' date (expected YYYY-MM-DD): "+err.Error())
			return
		}
		cutoff = parsed
	} else {
		cutoff = time.Now().AddDate(0, 0, -days)
	}

	collIDs, itemIDs, err := s.projectIntelVisibility(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	items := s.listTerminalItemsSince(workspaceID, collIDs, itemIDs, cutoff, 100)
	if parent != "" {
		// The parent filter needs the items' own parent-link metadata
		// (parent_link_id / parent_ref / parent_title) populated —
		// batch-enrich before filtering, same helper handleListItems uses.
		s.enrichItemsWithParent(workspaceID, items, collIDs)
		filtered := items[:0]
		for _, item := range items {
			if itemMatchesParentFilter(item, parent) {
				filtered = append(filtered, item)
			}
		}
		items = filtered
	}

	// Group by collection slug, preserving first-seen ordering (matches the
	// CLI's groupOrder slice / dispatchProjectChangelog's identical approach).
	groupOrder := make([]string, 0)
	groups := make(map[string]*ChangelogGroup)
	for _, item := range items {
		key := item.CollectionSlug
		if key == "" {
			key = "other"
		}
		g, exists := groups[key]
		if !exists {
			name := item.CollectionName
			if name == "" {
				name = key
			}
			g = &ChangelogGroup{Collection: name, Icon: item.CollectionIcon, Items: []ChangelogItem{}}
			groups[key] = g
			groupOrder = append(groupOrder, key)
		}
		g.Items = append(g.Items, ChangelogItem{
			Ref:    item.Ref,
			Title:  item.Title,
			Status: extractFieldValue(item.Fields, "status"),
		})
		g.Count = len(g.Items)
	}

	periodLabel := "last " + strconv.Itoa(days) + " days"
	if since != "" {
		periodLabel = "since " + since
	}
	if parent != "" {
		periodLabel += " (parent: " + parent + ")"
	}

	resp := ChangelogResponse{
		Period: periodLabel,
		Since:  cutoff.Format("2006-01-02"),
		Total:  len(items),
		Groups: make([]ChangelogGroup, 0, len(groupOrder)),
	}
	for _, key := range groupOrder {
		resp.Groups = append(resp.Groups, *groups[key])
	}

	writeJSON(w, http.StatusOK, resp)
}
