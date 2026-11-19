package server

import (
	"encoding/json"
	"log/slog"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/needs"
)

// unroutedAsk reports whether an item, as stored, is an open ask that reaches
// no person: the Needs you router would leave it in its agent role's queue
// (needs.WaitingOn's role rule), so no human sees it. It builds the item's
// candidate the way needsForWorkspace does, without a due reminder: a due
// reminder is per viewer and is not a route for an ask.
func (s *Server) unroutedAsk(workspaceID string, item *models.Item) (bool, error) {
	if item == nil || item.AgentRoleID == nil || *item.AgentRoleID == "" {
		return false, nil // the role rule needs a role
	}
	coll, err := s.store.GetCollection(item.CollectionID)
	if err != nil || coll == nil || coll.IsSystem {
		return false, err
	}
	ci, _ := newNeedsCollInfo(*coll)
	var fields map[string]any
	if item.Fields != "" {
		_ = json.Unmarshal([]byte(item.Fields), &fields)
	}
	if models.IsTerminalItem(fields, ci.schema, ci.settings) {
		return false, nil
	}
	c := needs.Candidate{
		ItemID: item.ID, CollectionID: item.CollectionID, AwaitsHuman: ci.awaits,
		WaitingStatus: ci.isWaiting(fields), AgentRoleID: *item.AgentRoleID,
		AssignedAt: item.CreatedAt, CreatedAt: item.CreatedAt,
	}
	if !c.Asked() {
		return false, nil
	}
	if item.AssignedUserID != nil {
		c.AssigneeUserID = *item.AssignedUserID
	}
	mentions, err := s.store.OpenMentionsForItems([]string{item.ID})
	if err != nil {
		return false, err
	}
	for _, m := range mentions[item.ID] {
		c.Mentions = append(c.Mentions, needs.Mention{UserID: m.UserID, CommentID: m.CommentID, By: m.By, At: m.At, CommentAt: m.CommentAt})
	}
	elig := &needsEligibility{s: s, wsID: workspaceID, cache: map[string]*needsVisibility{}}
	t := needs.WaitingOn(c, elig)
	if elig.err != nil {
		return false, elig.err
	}
	return t.Kind == needs.TargetRole, nil
}

// attachUnroutedAsk sets warnings.unrouted_ask on a stored item's response,
// merging into any warnings already there. The write has committed, so a
// store error here is logged and the warning left unset.
func (s *Server) attachUnroutedAsk(workspaceID string, item *models.Item) {
	unrouted, err := s.unroutedAsk(workspaceID, item)
	if err != nil {
		slog.Warn("unrouted ask warning not computed", "item_id", item.ID, "workspace_id", workspaceID, "error", err)
		return
	}
	if !unrouted {
		return
	}
	if item.Warnings == nil {
		item.Warnings = &models.ItemWriteWarnings{}
	}
	item.Warnings.UnroutedAsk = true
}
