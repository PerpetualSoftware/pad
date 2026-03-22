package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// Dashboard response types

type DashboardResponse struct {
	Summary        DashboardSummary       `json:"summary"`
	ActivePhases   []DashboardPhase       `json:"active_phases"`
	Attention      []DashboardAttention   `json:"attention"`
	RecentActivity []DashboardActivity    `json:"recent_activity"`
	SuggestedNext  []DashboardSuggestion  `json:"suggested_next"`
}

type DashboardActivity struct {
	Action         string `json:"action"`
	Actor          string `json:"actor"`
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

type DashboardPhase struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Progress  int    `json:"progress"`
	TaskCount int    `json:"task_count"`
	DoneCount int    `json:"done_count"`
}

type DashboardAttention struct {
	Type       string `json:"type"`
	ItemSlug   string `json:"item_slug"`
	ItemTitle  string `json:"item_title"`
	Collection string `json:"collection"`
	Reason     string `json:"reason"`
}

type DashboardSuggestion struct {
	ItemSlug   string `json:"item_slug"`
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

// isDoneStatus returns true if the status indicates a completed item.
func isDoneStatus(status string) bool {
	switch strings.ToLower(status) {
	case "done", "completed", "resolved":
		return true
	default:
		return false
	}
}

func (s *Server) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	resp := DashboardResponse{
		Summary: DashboardSummary{
			ByCollection: make(map[string]map[string]int),
		},
		ActivePhases:   []DashboardPhase{},
		Attention:      []DashboardAttention{},
		RecentActivity: []DashboardActivity{},
		SuggestedNext:  []DashboardSuggestion{},
	}

	// Summary: items grouped by collection slug and status field
	allItems, err := s.store.ListItems(workspaceID, models.ItemListParams{})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
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

	// Active phases: items in "phases" collection where status=active
	phases, err := s.store.ListItems(workspaceID, models.ItemListParams{
		CollectionSlug: "phases",
		Fields:         map[string]string{"status": "active"},
	})
	if err == nil {
		for _, phase := range phases {
			dp := DashboardPhase{
				Slug:  phase.Slug,
				Title: phase.Title,
			}

			// Compute progress from tasks linked via relation field
			total, done, err := s.store.GetPhaseProgress(phase.ID)
			if err == nil && total > 0 {
				dp.TaskCount = total
				dp.DoneCount = done
				dp.Progress = (done * 100) / total
			} else {
				// Fallback: use explicit progress field on the phase
				progress := extractFieldValue(phase.Fields, "progress")
				if progress != "" {
					var pval float64
					if err := json.Unmarshal([]byte(progress), &pval); err == nil {
						dp.Progress = int(pval)
					}
				}
			}

			resp.ActivePhases = append(resp.ActivePhases, dp)
		}
	}

	// --- Attention ---

	now := time.Now()
	staleCutoff := now.Add(-3 * 24 * time.Hour)

	// (a) Stalled: status is in-progress or in_progress, updated_at older than 3 days
	for _, statusVal := range []string{"in-progress", "in_progress"} {
		items, err := s.store.ListItems(workspaceID, models.ItemListParams{
			Fields: map[string]string{"status": statusVal},
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
		if isDoneStatus(status) {
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
					ItemTitle:  item.Title,
					Collection: item.CollectionSlug,
					Reason:     strings.ReplaceAll(dateField, "_", " ") + " was " + dateVal,
				})
				break // only report once per item even if both fields are overdue
			}
		}
	}

	// (c) Phase completion: phases where ALL linked tasks are done
	for _, dp := range resp.ActivePhases {
		if dp.TaskCount > 0 && dp.DoneCount == dp.TaskCount {
			resp.Attention = append(resp.Attention, DashboardAttention{
				Type:       "phase_completion",
				ItemSlug:   dp.Slug,
				ItemTitle:  dp.Title,
				Collection: "phases",
				Reason:     "All " + strconv.Itoa(dp.TaskCount) + " tasks are done. Mark as completed?",
			})
		}
	}

	// (d) Orphaned tasks: tasks with no phase relation set
	//     Only flag these if the workspace actually has phases with tasks linked to them.
	hasPhaseWithTasks := false
	for _, dp := range resp.ActivePhases {
		if dp.TaskCount > 0 {
			hasPhaseWithTasks = true
			break
		}
	}
	if hasPhaseWithTasks {
		allTasks, err := s.store.ListItems(workspaceID, models.ItemListParams{
			CollectionSlug: "tasks",
		})
		if err == nil {
			for _, task := range allTasks {
				status := extractFieldValue(task.Fields, "status")
				if isDoneStatus(status) {
					continue
				}
				phaseRef := extractFieldValue(task.Fields, "phase")
				if phaseRef == "" {
					resp.Attention = append(resp.Attention, DashboardAttention{
						Type:       "orphaned_task",
						ItemSlug:   task.Slug,
						ItemTitle:  task.Title,
						Collection: "tasks",
						Reason:     "Task has no phase assigned",
					})
				}
			}
		}
	}

	// Recent activity — enriched with item titles
	activities, err := s.store.ListWorkspaceActivity(workspaceID, models.ActivityListParams{
		Limit: 10,
	})
	if err == nil && activities != nil {
		for _, a := range activities {
			da := DashboardActivity{
				Action:    a.Action,
				Actor:     a.Actor,
				Source:    a.Source,
				CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z"),
				Metadata:  a.Metadata,
			}
			// Look up item title if we have a document/item ID
			if a.DocumentID != "" {
				item, err := s.store.GetItem(a.DocumentID)
				if err == nil && item != nil {
					da.ItemTitle = item.Title
					da.ItemSlug = item.Slug
					da.CollectionSlug = item.CollectionSlug
				}
			}
			resp.RecentActivity = append(resp.RecentActivity, da)
		}
	}

	// --- Suggested Next ---
	// Find open tasks in active phases, sorted by priority, top 3
	type suggestion struct {
		item     models.Item
		phase    string
		priority int
	}
	var candidates []suggestion

	for _, dp := range resp.ActivePhases {
		phaseItem, err := s.store.GetItemBySlug(workspaceID, dp.Slug)
		if err != nil || phaseItem == nil {
			continue
		}
		tasks, err := s.store.GetTasksForPhase(phaseItem.ID)
		if err != nil {
			continue
		}
		for _, task := range tasks {
			taskStatus := extractFieldValue(task.Fields, "status")
			if taskStatus == "open" {
				pri := extractFieldValue(task.Fields, "priority")
				candidates = append(candidates, suggestion{
					item:     task,
					phase:    dp.Title,
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
		reason := "Open task in active phase \"" + c.phase + "\""
		if pri != "" {
			reason += " (" + pri + " priority)"
		}
		resp.SuggestedNext = append(resp.SuggestedNext, DashboardSuggestion{
			ItemSlug:   c.item.Slug,
			ItemTitle:  c.item.Title,
			Collection: "tasks",
			Reason:     reason,
		})
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
