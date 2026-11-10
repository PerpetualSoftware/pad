package store

import (
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// importScheduleGatesTx validates the whole gate graph before inserting any
// gates. A missing or refused schedule cannot be replaced with a status gate:
// the captured occurrence and opening are restored exactly, without evaluation.
func (s *Store) importScheduleGatesTx(tx *sql.Tx, workspaceID string, gates []models.ScheduleGateExport, items, schedules map[string]string, sources map[string]models.ScheduleExport) error {
	ids := map[string]bool{}
	pairs := map[[2]string]bool{}
	children := map[string][]string{}
	incoming := map[string]int{}
	for _, g := range gates {
		if g.ID == "" || ids[g.ID] {
			return invalidf("missing or duplicate schedule gate id %q", g.ID)
		}
		ids[g.ID] = true
		if items[g.BlockedItemID] == "" || items[g.BlockerItemID] == "" {
			return invalidf("schedule gate %q references an item that was not imported", g.ID)
		}
		if g.BlockedItemID == g.BlockerItemID {
			return invalidf("schedule gate %q cannot block itself", g.ID)
		}
		pair := [2]string{g.BlockedItemID, g.BlockerItemID}
		if pairs[pair] {
			return invalidf("duplicate schedule gate pair on gate %q", g.ID)
		}
		pairs[pair] = true
		if g.BlockerScheduleID != nil {
			id := *g.BlockerScheduleID
			source, ok := sources[id]
			if !ok || schedules[id] == "" || source.ItemID != g.BlockerItemID {
				return invalidf("schedule gate %q references a missing or foreign blocker schedule", g.ID)
			}
		}
		if !validLinkStamps(g.CreatedAt) || (g.OpenedAt != nil && !validLinkStamps(*g.OpenedAt)) {
			return invalidf("invalid timestamp on schedule gate %q", g.ID)
		}
		if strings.ContainsRune(g.BlockerOccurrenceKey, 0) || !utf8.ValidString(g.BlockerOccurrenceKey) {
			return invalidf("invalid occurrence key on schedule gate %q", g.ID)
		}
		children[g.BlockerItemID] = append(children[g.BlockerItemID], g.BlockedItemID)
		if _, ok := incoming[g.BlockerItemID]; !ok {
			incoming[g.BlockerItemID] = 0
		}
		incoming[g.BlockedItemID]++
	}
	// Kahn traversal checks cycles in linear time without a recursion depth limit.
	var ready []string
	for item, n := range incoming {
		if n == 0 {
			ready = append(ready, item)
		}
	}
	visited := 0
	for len(ready) > 0 {
		item := ready[0]
		ready = ready[1:]
		visited++
		for _, child := range children[item] {
			incoming[child]--
			if incoming[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if visited != len(incoming) {
		return invalidf("schedule gates contain a cycle")
	}
	for _, g := range gates {
		var sc any
		if g.BlockerScheduleID != nil {
			sc = schedules[*g.BlockerScheduleID]
		}
		if _, err := tx.Exec(s.q(`INSERT INTO schedule_gates
   (id,workspace_id,blocked_item_id,blocker_item_id,blocker_schedule_id,blocker_occurrence_key,opened_at,created_at)
   VALUES (?,?,?,?,?,?,?,?)`), newID(), workspaceID, items[g.BlockedItemID], items[g.BlockerItemID], sc, g.BlockerOccurrenceKey, g.OpenedAt, g.CreatedAt); err != nil {
			return fmt.Errorf("import schedule gate: %w", err)
		}
	}
	return nil
}
