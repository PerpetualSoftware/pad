package server

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

const (
	defaultMeAgentsFinished = 5
	maxMeAgentsFinished     = 20
	meAgentsFinishedWindow  = 30 * 24 * time.Hour
)

type meAgentsFinishedItem struct {
	Workspace     string `json:"workspace"`
	OwnerUsername string `json:"owner_username"`
	Ref           string `json:"ref"`
	Collection    string `json:"collection"`
	Title         string `json:"title"`
	Value         string `json:"value"`
	CompletedAt   string `json:"completed_at"`
	completedAt   time.Time
}

type meAgentsSummary struct {
	NowCount   int    `json:"now_count"`
	QueueTotal int    `json:"queue_total"`
	Parked     string `json:"parked"`
}

type meAgentGroupResponse struct {
	Slug     string                     `json:"slug"`
	Name     string                     `json:"name"`
	Icon     string                     `json:"icon"`
	Lanes    []models.RoleQueueResponse `json:"lanes"`
	Finished []meAgentsFinishedItem     `json:"finished"`
	Summary  meAgentsSummary            `json:"summary"`
}

type meAgentsResponse struct {
	Agents   []meAgentGroupResponse `json:"agents"`
	Degraded []string               `json:"degraded"`
}

type meAgentsVisibility struct {
	unrestricted bool
	collections  map[string]bool
	items        map[string]bool
}

func (v meAgentsVisibility) allows(collectionID, itemID string) bool {
	if v.unrestricted {
		return true
	}
	return v.collections[collectionID] || v.items[itemID]
}

func buildMeAgentsVisibility(fullCollIDs, grantedItemIDs []string) meAgentsVisibility {
	if fullCollIDs == nil && grantedItemIDs == nil {
		return meAgentsVisibility{unrestricted: true}
	}
	v := meAgentsVisibility{
		collections: make(map[string]bool, len(fullCollIDs)),
		items:       make(map[string]bool, len(grantedItemIDs)),
	}
	for _, id := range fullCollIDs {
		v.collections[id] = true
	}
	for _, id := range grantedItemIDs {
		v.items[id] = true
	}
	return v
}

func (s *Server) meAgentsQueryer(r *http.Request) store.Queryer {
	if s.meAgentsQueryerOverride != nil {
		return s.meAgentsQueryerOverride(r)
	}
	return s.store.QContext(r.Context())
}

// handleMeAgents returns the caller's cross-workspace agents overview:
// GET /api/v1/me/agents?finished=5&queue=5&workspace=<slug>
func (s *Server) handleMeAgents(w http.ResponseWriter, r *http.Request) {
	// Gate 1: app principal refused identically to /me/needs before anything else.
	if appContextFrom(r) != nil {
		writeError(w, http.StatusNotFound, "not_found", "Not found")
		return
	}
	// Gate 2: signed-in user required.
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Sign in to see your agents")
		return
	}
	// Gate 3: strict query parameter validation.
	finishedLimit, validFinished := parseStrictBoundedIntQueryParam(r, "finished", defaultMeAgentsFinished, 0, maxMeAgentsFinished)
	if !validFinished {
		writeError(w, http.StatusBadRequest, "validation_error", "finished must be an integer between 0 and 20")
		return
	}
	queueLimit, validQueue := parseStrictBoundedIntQueryParam(r, "queue", defaultRoleQueueLimit, 1, maxRoleQueueLimit)
	if !validQueue {
		writeError(w, http.StatusBadRequest, "validation_error", "queue must be an integer between 1 and 50")
		return
	}
	wsFilter := r.URL.Query().Get("workspace")

	w.Header().Set("Cache-Control", "private, no-store")

	q := s.meAgentsQueryer(r)
	admitted, memberships, err := s.store.ListUserAdmittedWorkspacesAndMembershipsQ(q, user.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	admitted = filterWorkspacesByTokenAllowlist(r.Context(), admitted)
	if id := tokenWorkspaceID(r); id != "" {
		kept := admitted[:0]
		for _, ws := range admitted {
			if ws.ID == id {
				kept = append(kept, ws)
			}
		}
		admitted = kept
	}
	if wsFilter != "" {
		var filtered []models.Workspace
		for _, ws := range admitted {
			if ws.Slug == wsFilter {
				filtered = append(filtered, ws)
				break
			}
		}
		admitted = filtered
	}
	if len(admitted) == 0 {
		writeJSON(w, http.StatusOK, meAgentsResponse{
			Agents:   []meAgentGroupResponse{},
			Degraded: []string{},
		})
		return
	}

	type pendingLane struct {
		wsSlug             string
		wsName             string
		wsOwner            string
		role               models.AgentRole
		nowEntries         []store.RoleLaneEntry
		queueEntries       []store.RoleLaneEntry
		leases             map[string]models.ItemLease
		firstActiveBlocker map[string]activeBlocker
		canReorder         bool
		canPark            bool
		canEdit            func(itemID, collectionID string) bool
	}

	type fallbackRoleMeta struct {
		wsName string
		wsSlug string
		role   models.AgentRole
	}

	type agentAccumulator struct {
		slug          string
		pendingLanes  []pendingLane
		finished      []meAgentsFinishedItem
		fallbackRoles []fallbackRoleMeta
	}

	groups := make(map[string]*agentAccumulator)
	getGroup := func(slug string) *agentAccumulator {
		g, ok := groups[slug]
		if !ok {
			g = &agentAccumulator{slug: slug}
			groups[slug] = g
		}
		return g
	}

	degraded := []string{}
	finishedSince := s.agendaNow().UTC().Add(-meAgentsFinishedWindow)
	needPresence := false

	for i := range admitted {
		ws := admitted[i]

		roles, err := s.store.ListAgentRolesForOverviewQ(q, ws.ID)
		if err != nil {
			slog.Error("me/agents: list roles failed; workspace degraded", "workspace_id", ws.ID, "workspace_slug", ws.Slug, "error", err)
			degraded = append(degraded, ws.Slug)
			continue
		}
		if len(roles) == 0 {
			continue
		}

		member, isMember := memberships[ws.ID]
		var memberPtr *models.WorkspaceMember
		if isMember {
			memberPtr = &member
		}

		// Visibility: same per-user backlinks rule as needsEligibility.visibility
		// (platform admin counted by membership only, bearer-style), reusing the
		// already-loaded user and membership rows.
		fullCollIDs, grantedItemIDs, memberColls, err := s.store.ResolveBacklinksVisibilityWithLoadedQ(q, user, memberPtr, ws.ID, false, true)
		if err != nil {
			slog.Error("me/agents: resolve visibility failed; workspace degraded", "workspace_id", ws.ID, "workspace_slug", ws.Slug, "error", err)
			degraded = append(degraded, ws.Slug)
			continue
		}
		vis := buildMeAgentsVisibility(fullCollIDs, grantedItemIDs)

		// Capabilities from membership role + collection access (grants ignored for can_edit).
		isEditorOrOwner := isMember && (member.Role == "owner" || member.Role == "editor")
		canReorder := isEditorOrOwner
		canPark := canReorder && vis.unrestricted
		var canEdit func(itemID, collectionID string) bool
		if !isEditorOrOwner {
			canEdit = func(string, string) bool { return false }
		} else if member.CollectionAccess == "all" || member.CollectionAccess == "" {
			canEdit = func(string, string) bool { return true }
		} else {
			editSet := make(map[string]bool, len(memberColls))
			for _, cid := range memberColls {
				editSet[cid] = true
			}
			canEdit = func(_, collectionID string) bool {
				return editSet[collectionID]
			}
		}

		lanesByRole, collsByID, err := s.store.LoadWorkspaceRoleLanesQ(q, ws.ID)
		if err != nil {
			slog.Error("me/agents: load workspace role lanes failed; workspace degraded", "workspace_id", ws.ID, "workspace_slug", ws.Slug, "error", err)
			degraded = append(degraded, ws.Slug)
			continue
		}
		// Build the live lease map directly from the lane rows already loaded
		// (same liveness predicate and columns as ListItemLeasesQ, saving one
		// round trip per workspace).
		leases := make(map[string]models.ItemLease)
		for _, lane := range lanesByRole {
			for _, e := range lane {
				if e.Lease != nil {
					leases[e.Item.ID] = *e.Lease
				}
			}
		}
		blockers, err := s.firstActiveBlockersWithVisibilityQ(q, ws.ID, vis.allows)
		if err != nil {
			slog.Error("me/agents: blockers failed; workspace degraded", "workspace_id", ws.ID, "workspace_slug", ws.Slug, "error", err)
			degraded = append(degraded, ws.Slug)
			continue
		}

		roleByID := make(map[string]models.AgentRole, len(roles))
		roleIDs := make([]string, len(roles))
		for idx, role := range roles {
			roleByID[role.ID] = role
			roleIDs[idx] = role.ID
		}

		var finishedRows []store.WorkspaceRoleFinishedRow
		if finishedLimit > 0 {
			finishedFilters := buildFinishedCollectionFilters(collsByID)
			finishedVis := store.FinishedVisibility{
				Unrestricted:  vis.unrestricted,
				CollectionIDs: fullCollIDs,
				ItemIDs:       grantedItemIDs,
			}
			finishedRows, err = s.store.ListWorkspaceRoleFinishedItemsQ(q, ws.ID, roleIDs, finishedFilters, finishedVis, finishedSince, finishedLimit, finishedLimit*len(roleIDs))
			if err != nil {
				slog.Error("me/agents: finished items failed; workspace degraded", "workspace_id", ws.ID, "workspace_slug", ws.Slug, "error", err)
				degraded = append(degraded, ws.Slug)
				continue
			}
		}

		// All workspace reads succeeded — merge into groups.
		for _, role := range roles {
			nowEntries, queueEntries := partitionRoleLane(lanesByRole[role.ID], vis.allows)
			if len(nowEntries) == 0 && len(queueEntries) == 0 {
				continue
			}
			if !needPresence && laneNeedsOwnPresence(nowEntries, leases, user.ID) {
				needPresence = true
			}
			g := getGroup(role.Slug)
			g.pendingLanes = append(g.pendingLanes, pendingLane{
				wsSlug:             ws.Slug,
				wsName:             ws.Name,
				wsOwner:            ws.OwnerUsername,
				role:               role,
				nowEntries:         nowEntries,
				queueEntries:       queueEntries,
				leases:             leases,
				firstActiveBlocker: blockers,
				canReorder:         canReorder,
				canPark:            canPark,
				canEdit:            canEdit,
			})
		}

		type finishedCollMeta struct {
			tracksWork    bool
			doneKey       string
			completedVals []string
		}
		collMetaCache := make(map[string]finishedCollMeta)
		seenFinished := make(map[string]bool)
		// Go-side visibility check is kept as a second guard (and filters rows
		// when the store falls back to 4x over-fetch for >500 visibility IDs).
		for _, row := range finishedRows {
			if seenFinished[row.ItemID] {
				continue
			}
			if !vis.allows(row.CollectionID, row.ItemID) {
				continue
			}
			role, ok := roleByID[row.AgentRoleID]
			if !ok {
				continue
			}
			meta, cached := collMetaCache[row.CollectionID]
			if !cached {
				meta.tracksWork = models.CollectionTracksWorkJSON(row.Settings)
				if meta.tracksWork {
					meta.doneKey, meta.completedVals = models.CollectionCompletedWorkValues(row.Schema, row.Settings)
				}
				collMetaCache[row.CollectionID] = meta
			}
			if !meta.tracksWork {
				continue
			}
			if row.FieldKey != meta.doneKey {
				continue
			}
			val := extractFieldValue(row.Fields, meta.doneKey)
			if !containsFoldValue(meta.completedVals, val) {
				continue
			}
			if !strings.EqualFold(row.ToStatus, val) {
				continue
			}
			seenFinished[row.ItemID] = true
			g := getGroup(role.Slug)
			g.fallbackRoles = append(g.fallbackRoles, fallbackRoleMeta{wsName: ws.Name, wsSlug: ws.Slug, role: role})
			g.finished = append(g.finished, meAgentsFinishedItem{
				Workspace:     ws.Slug,
				OwnerUsername: ws.OwnerUsername,
				Ref:           row.Ref,
				Collection:    collsByID[row.CollectionID].Slug,
				Title:         row.Title,
				Value:         val,
				CompletedAt:   row.CreatedAt.UTC().Format(time.RFC3339),
				completedAt:   row.CreatedAt,
			})
		}
	}

	// Load own session presence at most once per request, only when needed.
	var presence sessionPresenceSnapshot
	if needPresence {
		presence = s.loadOwnSessionPresence(user.ID)
	}

	// Resolve all park setter display names across all returned lanes in ONE query.
	var setterIDs []string
	for _, g := range groups {
		for _, pl := range g.pendingLanes {
			if pl.role.Park != nil && pl.role.Park.SetByUserID != nil && *pl.role.Park.SetByUserID != "" {
				setterIDs = append(setterIDs, *pl.role.Park.SetByUserID)
			}
		}
	}
	var setterNames map[string]string
	if len(setterIDs) > 0 {
		var nameErr error
		setterNames, nameErr = s.store.GetUserDisplayNamesQ(q, setterIDs)
		if nameErr != nil {
			slog.Warn("me/agents: failed to resolve park setter display names", "error", nameErr)
		}
	}

	type sortableAgent struct {
		minSortOrder int
		resp         meAgentGroupResponse
	}
	sortable := make([]sortableAgent, 0, len(groups))

	for _, g := range groups {
		if len(g.pendingLanes) == 0 && len(g.finished) == 0 {
			continue
		}

		// Sort lanes: lanes with Now items first, then workspace name, then workspace slug.
		sort.SliceStable(g.pendingLanes, func(i, j int) bool {
			iHasNow := len(g.pendingLanes[i].nowEntries) > 0
			jHasNow := len(g.pendingLanes[j].nowEntries) > 0
			if iHasNow != jHasNow {
				return iHasNow
			}
			if g.pendingLanes[i].wsName != g.pendingLanes[j].wsName {
				return g.pendingLanes[i].wsName < g.pendingLanes[j].wsName
			}
			return g.pendingLanes[i].wsSlug < g.pendingLanes[j].wsSlug
		})

		lanes := make([]models.RoleQueueResponse, 0, len(g.pendingLanes))
		nowCount := 0
		queueTotal := 0
		anyParked := false
		allNonEmptyQueuesParkedAtTop := true
		minSortOrder := 0

		for idx, pl := range g.pendingLanes {
			roleCopy := pl.role
			if roleCopy.Park != nil && roleCopy.Park.SetByUserID != nil && setterNames != nil {
				if displayName, ok := setterNames[*roleCopy.Park.SetByUserID]; ok && displayName != "" {
					pCopy := *roleCopy.Park
					dn := displayName
					pCopy.SetBy = &dn
					roleCopy.Park = &pCopy
				}
			}
			laneResp := buildRoleQueueLaneResponse(
				pl.wsSlug,
				pl.wsOwner,
				&roleCopy,
				pl.nowEntries,
				pl.queueEntries,
				pl.leases,
				pl.firstActiveBlocker,
				queueLimit,
				user.ID,
				presence,
				pl.canReorder,
				pl.canPark,
				pl.canEdit,
			)
			lanes = append(lanes, laneResp)

			if idx == 0 || pl.role.SortOrder < minSortOrder {
				minSortOrder = pl.role.SortOrder
			}
			nowCount += len(laneResp.Now)
			queueTotal += laneResp.QueueTotal
			if laneResp.ParkedTotal > 0 {
				anyParked = true
			}
			if laneResp.QueueTotal > 0 {
				if laneResp.Park == nil || laneResp.Park.Index == nil || *laneResp.Park.Index != 0 {
					allNonEmptyQueuesParkedAtTop = false
				}
			}
		}

		var name, icon string
		if len(lanes) > 0 {
			name = lanes[0].Role.Name
			icon = lanes[0].Role.Icon
		} else {
			sort.SliceStable(g.fallbackRoles, func(i, j int) bool {
				if g.fallbackRoles[i].wsName != g.fallbackRoles[j].wsName {
					return g.fallbackRoles[i].wsName < g.fallbackRoles[j].wsName
				}
				return g.fallbackRoles[i].wsSlug < g.fallbackRoles[j].wsSlug
			})
			name = g.fallbackRoles[0].role.Name
			icon = g.fallbackRoles[0].role.Icon
			minSortOrder = g.fallbackRoles[0].role.SortOrder
			for _, fr := range g.fallbackRoles[1:] {
				if fr.role.SortOrder < minSortOrder {
					minSortOrder = fr.role.SortOrder
				}
			}
		}

		// Sort finished newest first, cap at finishedLimit.
		finished := g.finished
		sort.SliceStable(finished, func(i, j int) bool {
			if !finished[i].completedAt.Equal(finished[j].completedAt) {
				return finished[i].completedAt.After(finished[j].completedAt)
			}
			if finished[i].Workspace != finished[j].Workspace {
				return finished[i].Workspace < finished[j].Workspace
			}
			return finished[i].Ref < finished[j].Ref
		})
		if len(finished) > finishedLimit {
			finished = finished[:finishedLimit]
		}
		if finished == nil {
			finished = []meAgentsFinishedItem{}
		}

		parkedSummary := "none"
		if anyParked {
			if allNonEmptyQueuesParkedAtTop {
				parkedSummary = "all"
			} else {
				parkedSummary = "some"
			}
		}

		sortable = append(sortable, sortableAgent{
			minSortOrder: minSortOrder,
			resp: meAgentGroupResponse{
				Slug:     g.slug,
				Name:     name,
				Icon:     icon,
				Lanes:    lanes,
				Finished: finished,
				Summary: meAgentsSummary{
					NowCount:   nowCount,
					QueueTotal: queueTotal,
					Parked:     parkedSummary,
				},
			},
		})
	}

	// Order agents: minimum role sort_order across lanes, then name, then slug.
	sort.SliceStable(sortable, func(i, j int) bool {
		if sortable[i].minSortOrder != sortable[j].minSortOrder {
			return sortable[i].minSortOrder < sortable[j].minSortOrder
		}
		if sortable[i].resp.Name != sortable[j].resp.Name {
			return sortable[i].resp.Name < sortable[j].resp.Name
		}
		return sortable[i].resp.Slug < sortable[j].resp.Slug
	})

	agents := make([]meAgentGroupResponse, len(sortable))
	for i, sa := range sortable {
		agents[i] = sa.resp
	}
	sort.Strings(degraded)

	writeJSON(w, http.StatusOK, meAgentsResponse{
		Agents:   agents,
		Degraded: degraded,
	})
}

func buildFinishedCollectionFilters(colls map[string]store.RoleLaneCollectionInfo) []store.FinishedCollectionFilter {
	ids := make([]string, 0, len(colls))
	for id, ci := range colls {
		if ci.TracksWork {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	filters := make([]store.FinishedCollectionFilter, 0, len(ids))
	for _, id := range ids {
		ci := colls[id]
		doneKey, completedVals := models.CollectionCompletedWorkValues(ci.RawSchema, ci.RawSettings)
		if doneKey == "" || len(completedVals) == 0 {
			continue
		}
		filters = append(filters, store.FinishedCollectionFilter{
			CollectionID: id,
			FieldKey:     doneKey,
			Values:       needsWaitingSpellings(ci.Schema, doneKey, completedVals),
		})
	}
	return filters
}

func containsFoldValue(values []string, s string) bool {
	if s == "" {
		return false
	}
	for _, v := range values {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
