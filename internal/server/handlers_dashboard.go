package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/models"
	"github.com/xarmian/pad/internal/store"
)

// Dashboard response types

type DashboardResponse struct {
	Summary        DashboardSummary       `json:"summary"`
	ActiveItems    []DashboardActiveItem  `json:"active_items"`
	ActivePlans    []DashboardPlan         `json:"active_plans"`
	StarredItems   []DashboardActiveItem  `json:"starred_items,omitempty"`
	ByRole         []store.RoleBreakdown  `json:"by_role,omitempty"`
	Attention      []DashboardAttention   `json:"attention"`
	RecentActivity []DashboardActivity    `json:"recent_activity"`
	SuggestedNext  []DashboardSuggestion  `json:"suggested_next"`
}

type DashboardActiveItem struct {
	Slug           string `json:"slug"`
	Title          string `json:"title"`
	CollectionSlug string `json:"collection_slug"`
	CollectionIcon string `json:"collection_icon"`
	Priority       string `json:"priority,omitempty"`
	Status         string `json:"status"`
	UpdatedAt      string `json:"updated_at"`
	ItemRef        string `json:"item_ref,omitempty"`
}

type DashboardActivity struct {
	Action         string `json:"action"`
	Actor          string `json:"actor"`
	ActorName      string `json:"actor_name,omitempty"`
	Source         string `json:"source"`
	CreatedAt      string `json:"created_at"`
	ItemTitle      string `json:"item_title,omitempty"`
	ItemSlug       string `json:"item_slug,omitempty"`
	CollectionSlug string `json:"collection_slug,omitempty"`
	Metadata       string `json:"metadata,omitempty"`
}

type DashboardSummary struct {
	TotalItems   int                       `json:"total_items"`
	ByCollection map[string]map[string]int `json:"by_collection"`
}

type DashboardPlan struct {
	Slug      string `json:"slug"`
	Ref       string `json:"ref,omitempty"`
	Title     string `json:"title"`
	Progress  int    `json:"progress"`
	TaskCount int    `json:"task_count"`
	DoneCount int    `json:"done_count"`
}

type DashboardAttention struct {
	Type       string `json:"type"`
	ItemSlug   string `json:"item_slug"`
	ItemRef    string `json:"item_ref,omitempty"`
	ItemTitle  string `json:"item_title"`
	Collection string `json:"collection"`
	Reason     string `json:"reason"`
}

type DashboardSuggestion struct {
	ItemSlug   string `json:"item_slug"`
	ItemRef    string `json:"item_ref,omitempty"`
	ItemTitle  string `json:"item_title"`
	Collection string `json:"collection"`
	Reason     string `json:"reason"`
}

// priorityRank returns a sort rank for task priority (lower = higher priority).
func priorityRank(priority string) int {
	switch strings.ToLower(priority) {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
	}
}

// isActiveStatus returns true if the status indicates work actively in progress.
// It excludes both initial/queued states and terminal/completed states.
func isActiveStatus(status string) bool {
	s := strings.ToLower(strings.ReplaceAll(status, "-", "_"))
	switch s {
	// Active statuses
	case "in_progress", "exploring", "fixing", "confirmed", "in_review":
		return true
	default:
		return false
	}
}

// buildSchemaMap builds a map of collection ID → parsed CollectionSchema for quick lookups.
func buildSchemaMap(collections []models.Collection) map[string]models.CollectionSchema {
	m := make(map[string]models.CollectionSchema, len(collections))
	for _, c := range collections {
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(c.Schema), &schema); err == nil {
			m[c.ID] = schema
		}
	}
	return m
}

// isItemTerminal checks if an item's status is terminal using its collection's schema.
// Falls back to default terminal statuses if the collection is not in the schema map.
func isItemTerminal(status, collectionID string, schemaMap map[string]models.CollectionSchema) bool {
	if schema, ok := schemaMap[collectionID]; ok {
		return models.IsTerminalStatus(status, schema)
	}
	return models.IsTerminalStatusDefault(status)
}

func (s *Server) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	// Compute collection visibility once for the entire dashboard
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// For guests, compute item-level grant filtering
	dashFullCollIDs, dashGrantedItemIDs, dashGrantErr := s.guestResourceFilter(r, workspaceID)
	if dashGrantErr != nil {
		writeInternalError(w, dashGrantErr)
		return
	}
	dashGrantedItemSet := make(map[string]bool, len(dashGrantedItemIDs))
	for _, id := range dashGrantedItemIDs {
		dashGrantedItemSet[id] = true
	}
	dashFullCollSet := make(map[string]bool, len(dashFullCollIDs))
	for _, id := range dashFullCollIDs {
		dashFullCollSet[id] = true
	}

	// For users with item-level grants, use item-level filtering in ListItems queries
	dashCollIDs := visibleIDs
	var dashItemIDs []string
	if len(dashGrantedItemIDs) > 0 {
		dashCollIDs = dashFullCollIDs
		dashItemIDs = dashGrantedItemIDs
	}

	// Build a schema map for terminal status lookups
	collections, err := s.store.ListCollections(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	// Filter collections by visibility
	if visibleIDs != nil {
		filtered := make([]models.Collection, 0, len(collections))
		for _, c := range collections {
			if isCollectionVisible(c.ID, visibleIDs) {
				filtered = append(filtered, c)
			}
		}
		collections = filtered
	}
	schemaMap := buildSchemaMap(collections)

	resp := DashboardResponse{
		Summary: DashboardSummary{
			ByCollection: make(map[string]map[string]int),
		},
		ActiveItems:    []DashboardActiveItem{},
		ActivePlans:    []DashboardPlan{},
		Attention:      []DashboardAttention{},
		RecentActivity: []DashboardActivity{},
		SuggestedNext:  []DashboardSuggestion{},
	}

	// Summary: items grouped by collection slug and status field
	allItems, err := s.store.ListItems(workspaceID, models.ItemListParams{CollectionIDs: dashCollIDs, ItemIDs: dashItemIDs})
	if err != nil {
		writeInternalError(w, err)
		return
	}

	resp.Summary.TotalItems = len(allItems)
	for _, item := range allItems {
		collSlug := item.CollectionSlug
		if _, exists := resp.Summary.ByCollection[collSlug]; !exists {
			resp.Summary.ByCollection[collSlug] = make(map[string]int)
		}

		status := extractFieldValue(item.Fields, "status")
		if status == "" {
			status = "unknown"
		}
		resp.Summary.ByCollection[collSlug][status]++
	}

	// Active items: items currently being worked on (not initial state, not terminal state)
	for _, item := range allItems {
		status := extractFieldValue(item.Fields, "status")
		if !isActiveStatus(status) {
			continue
		}
		// Skip plans (they have their own section)
		if item.CollectionSlug == "plans" {
			continue
		}
		ai := DashboardActiveItem{
			Slug:           item.Slug,
			Title:          item.Title,
			CollectionSlug: item.CollectionSlug,
			CollectionIcon: item.CollectionIcon,
			Status:         status,
			UpdatedAt:      item.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		}
		ai.Priority = extractFieldValue(item.Fields, "priority")
		if item.CollectionPrefix != "" && item.ItemNumber != nil {
			ai.ItemRef = item.CollectionPrefix + "-" + strconv.Itoa(*item.ItemNumber)
		}
		resp.ActiveItems = append(resp.ActiveItems, ai)
	}
	// Sort active items: by priority rank then by most recently updated
	sort.Slice(resp.ActiveItems, func(i, j int) bool {
		pi := priorityRank(resp.ActiveItems[i].Priority)
		pj := priorityRank(resp.ActiveItems[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return resp.ActiveItems[i].UpdatedAt > resp.ActiveItems[j].UpdatedAt
	})
	// Cap at 10
	if len(resp.ActiveItems) > 10 {
		resp.ActiveItems = resp.ActiveItems[:10]
	}

	// Active plans: items in "plans" collection where status=active
	plans, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionSlug: "plans",
		CollectionIDs:  dashCollIDs,
		ItemIDs:        dashItemIDs,
		Fields:         map[string]string{"status": "active"},
	})
	if err == nil {
		for _, plan := range plans {
			dp := DashboardPlan{
				Slug:  plan.Slug,
				Ref:   plan.Ref,
				Title: plan.Title,
			}

			// Compute progress from visible child items only
			total, done := 0, 0
			if planChildren, cerr := s.store.GetChildItems(plan.ID); cerr == nil {
				for _, child := range planChildren {
					if !isCollectionVisible(child.CollectionID, visibleIDs) {
						continue
					}
					if !s.isItemVisibleToGuest(r, workspaceID, &child, dashFullCollIDs, dashGrantedItemIDs) {
						continue
					}
					total++
					childStatus := extractFieldValue(child.Fields, "status")
					if isItemTerminal(childStatus, child.CollectionID, schemaMap) {
						done++
					}
				}
			}
			if total > 0 {
				dp.TaskCount = total
				dp.DoneCount = done
				dp.Progress = (done * 100) / total
			} else {
				// Fallback: use explicit progress field on the plan
				progress := extractFieldValue(plan.Fields, "progress")
				if progress != "" {
					var pval float64
					if err := json.Unmarshal([]byte(progress), &pval); err == nil {
						dp.Progress = int(pval)
					}
				}
			}

			resp.ActivePlans = append(resp.ActivePlans, dp)
		}
	}

	// --- Attention ---

	now := time.Now()
	staleCutoff := now.Add(-3 * 24 * time.Hour)

	// (a) Stalled: status is in-progress or in_progress, updated_at older than 3 days
	for _, statusVal := range []string{"in-progress", "in_progress"} {
		items, err := s.store.ListItems(workspaceID, models.ItemListParams{
			CollectionIDs: dashCollIDs,
			ItemIDs:       dashItemIDs,
			Fields:        map[string]string{"status": statusVal},
		})
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.UpdatedAt.Before(staleCutoff) {
				daysSince := int(time.Since(item.UpdatedAt).Hours() / 24)
				resp.Attention = append(resp.Attention, DashboardAttention{
					Type:       "stalled",
					ItemSlug:   item.Slug,
					ItemRef:    item.Ref,
					ItemTitle:  item.Title,
					Collection: item.CollectionSlug,
					Reason:     "In progress for " + strconv.Itoa(daysSince) + " days with no updates",
				})
			}
		}
	}

	// (b) Overdue: items with a due_date or end_date in the past and status not done/completed/resolved
	todayStr := now.Format("2006-01-02")
	for _, item := range allItems {
		status := extractFieldValue(item.Fields, "status")
		if isItemTerminal(status, item.CollectionID, schemaMap) {
			continue
		}
		for _, dateField := range []string{"due_date", "end_date"} {
			dateVal := extractFieldValue(item.Fields, dateField)
			if dateVal == "" {
				continue
			}
			// Compare date strings lexicographically (YYYY-MM-DD format)
			if dateVal < todayStr {
				resp.Attention = append(resp.Attention, DashboardAttention{
					Type:       "overdue",
					ItemSlug:   item.Slug,
					ItemRef:    item.Ref,
					ItemTitle:  item.Title,
					Collection: item.CollectionSlug,
					Reason:     strings.ReplaceAll(dateField, "_", " ") + " was " + dateVal,
				})
				break // only report once per item even if both fields are overdue
			}
		}
	}

	// (c) Plan completion: plans where ALL child items are done
	for _, dp := range resp.ActivePlans {
		if dp.TaskCount > 0 && dp.DoneCount == dp.TaskCount {
			resp.Attention = append(resp.Attention, DashboardAttention{
				Type:       "plan_completion",
				ItemSlug:   dp.Slug,
				ItemTitle:  dp.Title,
				Collection: "plans",
				Reason:     "All " + strconv.Itoa(dp.TaskCount) + " tasks are done. Mark as completed?",
			})
		}
	}

	// (d) Orphaned tasks: tasks with no parent link set
	//     Only flag these if the workspace has active plans with children linked to them.
	hasParentWithChildren := false
	for _, dp := range resp.ActivePlans {
		if dp.TaskCount > 0 {
			hasParentWithChildren = true
			break
		}
	}
	if hasParentWithChildren {
		// Batch-fetch all child→parent mappings for efficiency
		parentMap, err := s.store.GetParentMap(workspaceID)
		if err != nil {
			parentMap = map[string]string{}
		}
		allTasks, err := s.store.ListItems(workspaceID, models.ItemListParams{
			CollectionSlug: "tasks",
			CollectionIDs:  dashCollIDs,
			ItemIDs:        dashItemIDs,
		})
		if err == nil {
			for _, task := range allTasks {
				status := extractFieldValue(task.Fields, "status")
				if isItemTerminal(status, task.CollectionID, schemaMap) {
					continue
				}
				if _, hasParent := parentMap[task.ID]; !hasParent {
					resp.Attention = append(resp.Attention, DashboardAttention{
						Type:       "orphaned_task",
						ItemSlug:   task.Slug,
						ItemRef:    task.Ref,
						ItemTitle:  task.Title,
						Collection: "tasks",
						Reason:     "Task has no plan assigned",
					})
				}
			}
		}
	}

	// (e) Blocked: non-done items that are blocked by other non-done items
	for _, item := range allItems {
		status := extractFieldValue(item.Fields, "status")
		if isItemTerminal(status, item.CollectionID, schemaMap) {
			continue
		}
		links, err := s.store.GetItemLinks(item.ID)
		if err != nil {
			continue
		}
		for _, link := range links {
			if link.LinkType != "blocks" {
				continue
			}
			// We care about links where this item is the target (i.e., blocked by source)
			if link.TargetID != item.ID {
				continue
			}
			// Check if the blocking item is still not done
			blocker, err := s.store.GetItem(link.SourceID)
			if err != nil || blocker == nil {
				continue
			}
			// Skip blockers from hidden collections or ungrantable items
			if !isCollectionVisible(blocker.CollectionID, visibleIDs) {
				continue
			}
			if !s.isItemVisibleToGuest(r, workspaceID, blocker, dashFullCollIDs, dashGrantedItemIDs) {
				continue
			}
			blockerStatus := extractFieldValue(blocker.Fields, "status")
			if isItemTerminal(blockerStatus, blocker.CollectionID, schemaMap) {
				continue
			}
			resp.Attention = append(resp.Attention, DashboardAttention{
				Type:       "blocked",
				ItemSlug:   item.Slug,
				ItemRef:    item.Ref,
				ItemTitle:  item.Title,
				Collection: item.CollectionSlug,
				Reason:     "Blocked by " + link.SourceTitle + " (still " + blockerStatus + ")",
			})
			break // only report the first active blocker per item
		}
	}

	// Recent activity — enriched with item titles and user names
	// Fetch more than needed since some may be filtered out by visibility
	activities, err := s.store.ListWorkspaceActivity(workspaceID, models.ActivityListParams{
		Limit: 30,
	})
	if err == nil && activities != nil {
		// Build visible slug set for filtering
		var visibleSlugSet map[string]bool
		if visibleIDs != nil {
			visibleSlugSet = make(map[string]bool)
			for _, id := range visibleIDs {
				coll, _ := s.store.GetCollection(id)
				if coll != nil {
					visibleSlugSet[coll.Slug] = true
				}
			}
		}

		for _, a := range activities {
			if len(resp.RecentActivity) >= 10 {
				break
			}
			da := DashboardActivity{
				Action:    a.Action,
				Actor:     a.Actor,
				ActorName: a.ActorName,
				Source:    a.Source,
				CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z"),
				Metadata:  a.Metadata,
			}
			// Look up item title if we have a document/item ID
			if a.DocumentID != "" {
				item, err := s.store.GetItem(a.DocumentID)
				if err == nil && item != nil {
					// Skip items in hidden collections
					if visibleSlugSet != nil && !visibleSlugSet[item.CollectionSlug] {
						continue
					}
					// For users with item grants: skip items not directly granted
					if !s.isItemVisibleToGuest(r, workspaceID, item, dashFullCollIDs, dashGrantedItemIDs) {
						continue
					}
					da.ItemTitle = item.Title
					da.ItemSlug = item.Slug
					da.CollectionSlug = item.CollectionSlug
				}
			} else if workspaceRole(r) == "guest" {
				// Workspace-level activity (no item) — skip for guests since
				// it may contain audit metadata (member invites, role changes).
				continue
			}
			resp.RecentActivity = append(resp.RecentActivity, da)
		}
	}

	// --- Suggested Next ---
	// Find open child items in active plans, sorted by priority, top 3
	type suggestion struct {
		item     models.Item
		plan     string
		priority int
	}
	var candidates []suggestion

	for _, dp := range resp.ActivePlans {
		parentItem, err := s.store.ResolveItem(workspaceID, dp.Slug)
		if err != nil || parentItem == nil {
			continue
		}
		tasks, err := s.store.GetChildItems(parentItem.ID)
		if err != nil {
			continue
		}
		for _, task := range tasks {
			// Skip tasks from hidden collections
			if !isCollectionVisible(task.CollectionID, visibleIDs) {
				continue
			}
			if !s.isItemVisibleToGuest(r, workspaceID, &task, dashFullCollIDs, dashGrantedItemIDs) {
				continue
			}
			taskStatus := extractFieldValue(task.Fields, "status")
			if taskStatus == "open" {
				pri := extractFieldValue(task.Fields, "priority")
				candidates = append(candidates, suggestion{
					item:     task,
					plan:     dp.Title,
					priority: priorityRank(pri),
				})
			}
		}
	}

	// Sort by priority rank (ascending = highest priority first)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].priority < candidates[j].priority
	})

	// Take top 3
	limit := 3
	if len(candidates) < limit {
		limit = len(candidates)
	}
	for _, c := range candidates[:limit] {
		pri := extractFieldValue(c.item.Fields, "priority")
		reason := "Open task in active plan \"" + c.plan + "\""
		if pri != "" {
			reason += " (" + pri + " priority)"
		}
		resp.SuggestedNext = append(resp.SuggestedNext, DashboardSuggestion{
			ItemSlug:   c.item.Slug,
			ItemRef:    c.item.Ref,
			ItemTitle:  c.item.Title,
			Collection: "tasks",
			Reason:     reason,
		})
	}

	// Role breakdown: items per role with assigned users.
	// When visibility is restricted, recompute from visible items only.
	if visibleIDs != nil {
		roleBreakdown, err := s.store.GetRoleBreakdown(workspaceID)
		if err == nil {
			// Recount from visible items
			roleCounts := make(map[string]int)
			roleUsers := make(map[string]map[string]bool)
			for _, item := range allItems {
				roleID := ""
				if item.AgentRoleID != nil {
					roleID = *item.AgentRoleID
				}
				status := extractFieldValue(item.Fields, "status")
				if isItemTerminal(status, item.CollectionID, schemaMap) {
					continue
				}
				roleCounts[roleID]++
				if item.AssignedUserName != "" {
					if roleUsers[roleID] == nil {
						roleUsers[roleID] = make(map[string]bool)
					}
					roleUsers[roleID][item.AssignedUserName] = true
				}
			}
			for i := range roleBreakdown {
				rid := ""
				if roleBreakdown[i].RoleID != nil {
					rid = *roleBreakdown[i].RoleID
				}
				roleBreakdown[i].ItemCount = roleCounts[rid]
				users := make([]string, 0)
				for u := range roleUsers[rid] {
					users = append(users, u)
				}
				roleBreakdown[i].Users = users
			}
			if len(roleBreakdown) > 0 {
				resp.ByRole = roleBreakdown
			}
		}
	} else {
		roleBreakdown, err := s.store.GetRoleBreakdown(workspaceID)
		if err == nil && len(roleBreakdown) > 0 {
			resp.ByRole = roleBreakdown
		}
	}

	// Starred items: non-terminal items starred by the current user
	if userID := currentUserID(r); userID != "" {
		starred, err := s.store.ListStarredItems(userID, workspaceID, false)
		if err == nil && len(starred) > 0 {
			starredItems := []DashboardActiveItem{}
			for _, item := range starred {
				// Apply same visibility filter as the rest of the dashboard
				if visibleIDs != nil && !isCollectionVisible(item.CollectionID, visibleIDs) {
					continue
				}
				if len(dashGrantedItemIDs) > 0 && !dashFullCollSet[item.CollectionID] && !dashGrantedItemSet[item.ID] {
					continue
				}
				si := DashboardActiveItem{
					Slug:           item.Slug,
					Title:          item.Title,
					CollectionSlug: item.CollectionSlug,
					CollectionIcon: item.CollectionIcon,
					Status:         extractFieldValue(item.Fields, "status"),
					UpdatedAt:      item.UpdatedAt.Format("2006-01-02T15:04:05Z"),
				}
				si.Priority = extractFieldValue(item.Fields, "priority")
				if item.CollectionPrefix != "" && item.ItemNumber != nil {
					si.ItemRef = item.CollectionPrefix + "-" + strconv.Itoa(*item.ItemNumber)
				}
				starredItems = append(starredItems, si)
				if len(starredItems) >= 10 {
					break
				}
			}
			if len(starredItems) > 0 {
				resp.StarredItems = starredItems
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// extractFieldValue extracts a string value from a JSON fields string.
func extractFieldValue(fieldsJSON, key string) string {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return ""
	}
	val, exists := fields[key]
	if !exists {
		return ""
	}
	switch v := val.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
