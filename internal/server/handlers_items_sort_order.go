package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// maxSortOrderUpdates bounds one reorder, the same cap /items/bulk uses.
const maxSortOrderUpdates = 1000

type sortOrderEntry struct {
	ID        string `json:"id"`
	SortOrder *int   `json:"sort_order"`
}

type sortOrderRequest struct {
	Updates []sortOrderEntry `json:"updates"`
}

type sortOrderItem struct {
	ID  string `json:"id"`
	Seq int64  `json:"seq"`
}

// handleItemsSortOrder is PUT /workspaces/{ws}/items/sort-order (TASK-3517):
// set sort_order for many items at once, all or nothing.
//
// The web reorder used to send one PATCH per row. Each spent a rate-limit
// token, and a refusal in the middle left the server holding a half-applied
// order. This takes {id, sort_order} PAIRS rather than an ordered list,
// because the client's plan (planLaneOrder) leaves view-only cards where they
// are and renumbers around them; a list would mean a dense renumber that
// rewrites cards the caller cannot edit.
//
// Permission is the PATCH's, per item and before any write: visible, then
// editable (the app write rule, then canEditInCollection, which is
// grant-aware). It is deliberately NOT /items/bulk's editor-role gate, which
// would refuse a guest who can reorder their granted items today. An item the
// caller cannot see answers exactly as an unknown one. One item the caller may
// not edit refuses the whole request (lead ruling, day 89).
//
// Each moved item gets its own "reordered" activity row, with its from/to and
// a reorder_batch id the feeds collapse on, so each row is filtered by the
// feed's per-row visibility rule like any other.
func (s *Server) handleItemsSortOrder(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	var req sortOrderRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}
	if len(req.Updates) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "updates is required")
		return
	}
	if len(req.Updates) > maxSortOrderUpdates {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("too many items: %d (max %d per request)", len(req.Updates), maxSortOrderUpdates))
		return
	}

	updates := make([]store.ItemSortUpdate, 0, len(req.Updates))
	items := make(map[string]*models.Item, len(req.Updates))
	for _, e := range req.Updates {
		if e.ID == "" || e.SortOrder == nil {
			writeError(w, http.StatusBadRequest, "bad_request", "each update needs an id and a sort_order")
			return
		}
		item, err := s.store.ResolveItem(workspaceID, e.ID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if item == nil {
			writeError(w, http.StatusNotFound, "not_found", "Item not found")
			return
		}
		// requireItemVisible answers an invisible item with the same 404.
		if !s.requireItemVisible(w, r, workspaceID, item) {
			return
		}
		// Two entries for one item are refused, however they were spelled
		// (an id and a ref resolve to the same row).
		if _, dup := items[item.ID]; dup {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("%s appears more than once", itemRefOrSlug(*item)))
			return
		}
		if !s.canReorderItem(w, r, workspaceID, item) {
			return
		}
		items[item.ID] = item
		updates = append(updates, store.ItemSortUpdate{ItemID: item.ID, SortOrder: *e.SortOrder})
	}

	batchID := uuid.NewString()
	results, err := s.store.UpdateItemSortOrders(workspaceID, updates, batchID)
	if err != nil {
		if errors.Is(err, store.ErrItemSortNotFound) {
			// Deleted between the checks and the write. Nothing was
			// written; the client reloads.
			writeError(w, http.StatusConflict, "conflict", "An item changed while reordering; nothing was applied. Reload and try again.")
			return
		}
		writeInternalError(w, err)
		return
	}

	actor, source := actorFromRequest(r)
	actorName := actorNameFromRequest(r)
	type collBatch struct {
		count  int
		maxSeq int64
	}
	byColl := map[string]*collBatch{}
	var changedIDs []string
	out := make([]sortOrderItem, 0, len(results))
	for _, res := range results {
		out = append(out, sortOrderItem{ID: res.ItemID, Seq: res.Seq})
		if !res.Changed {
			continue
		}
		item := items[res.ItemID]
		changedIDs = append(changedIDs, res.ItemID)
		s.logActivityWithMeta(workspaceID, res.ItemID, "reordered", r, auditMeta(map[string]string{
			"sort_order_from": strconv.Itoa(res.From),
			"sort_order_to":   strconv.Itoa(res.To),
			"reorder_batch":   batchID,
		}))
		b := byColl[item.CollectionSlug]
		if b == nil {
			b = &collBatch{}
			byColl[item.CollectionSlug] = b
		}
		b.count++
		if res.Seq > b.maxSeq {
			b.maxSeq = res.Seq
		}
	}
	for collSlug, b := range byColl {
		s.publishBulkItemsEvent(workspaceID, "reorder", collSlug, b.count, actor, actorName, source, b.maxSeq)
	}
	if len(changedIDs) > 0 {
		if err := s.store.EmitBulkHeaderEvent(workspaceID, batchID, "reorder", changedIDs, nil); err != nil {
			// Best-effort, as for /items/bulk: the members committed with
			// their own events and deliver individually without a header.
			slog.Error("failed to emit reorder batch header", "workspace_id", workspaceID, "batch_id", batchID, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// canReorderItem is requireEditPermission's decision, answered with the ref of
// the item that refused, since one refusal stops the whole reorder.
func (s *Server) canReorderItem(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item) bool {
	ok := appWriteAllows(r, item.CollectionID)
	if ok {
		var err error
		ok, err = s.canEditInCollection(r, workspaceID, item.ID, item.CollectionID)
		if err != nil {
			writeInternalError(w, err)
			return false
		}
	}
	if !ok {
		writeError2(w, http.StatusForbidden, "forbidden",
			fmt.Sprintf("You can't edit %s, so nothing was reordered", itemRefOrSlug(*item)),
			map[string]interface{}{"ref": itemRefOrSlug(*item)})
		return false
	}
	return true
}
