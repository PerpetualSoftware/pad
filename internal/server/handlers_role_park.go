package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

type setRoleParkRequest struct {
	At              *string `json:"at"`
	Before          *string `json:"before"`
	After           *string `json:"after"`
	ExpectedParkSeq *int    `json:"expected_park_seq"`
}

type clearRoleParkRequest struct {
	ExpectedParkSeq *int `json:"expected_park_seq"`
}

type roleParkResponse struct {
	Mode           string     `json:"mode"`
	SortOrder      *int       `json:"sort_order"`
	FirstParkedRef *string    `json:"first_parked_ref"`
	SetBy          *string    `json:"set_by"`
	SetAt          *time.Time `json:"set_at"`
	ParkSeq        int        `json:"park_seq"`
}

// decodeStrictSetRoleParkRequest decodes a single setRoleParkRequest JSON
// object through readBodyForDecode and decodeJSONBytes (size cap, NUL and
// repeated-member refusal, trailing data refused), then refuses unknown
// fields. An empty body is refused.
func decodeStrictSetRoleParkRequest(r *http.Request, req *setRoleParkRequest) error {
	raw, err := readBodyForDecode(r, defaultJSONBodyLimit)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return err
	}
	if len(bytes.Trim(raw, " \t\r\n")) == 0 {
		return errors.New("request body is required")
	}
	got, err := strictBodyBytes[setRoleParkRequest](raw)
	if err != nil {
		return err
	}
	*req = got
	return nil
}

// decodeStrictClearRoleParkRequest decodes an optional clearRoleParkRequest
// JSON object the same way. present is false for an absent or empty
// (JSON-whitespace-only) body, which the DELETE accepts.
func decodeStrictClearRoleParkRequest(r *http.Request, req *clearRoleParkRequest) (present bool, err error) {
	raw, err := readBodyForDecode(r, defaultJSONBodyLimit)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	if len(bytes.Trim(raw, " \t\r\n")) == 0 {
		return false, nil
	}
	got, err := strictBodyBytes[clearRoleParkRequest](raw)
	if err != nil {
		return false, err
	}
	*req = got
	return true, nil
}

// handleSetRolePark places or moves a role lane's park line:
// PUT /api/v1/workspaces/{slug}/agent-roles/{roleID}/park
func (s *Server) handleSetRolePark(w http.ResponseWriter, r *http.Request) {
	// Gate 1: workspace middleware (existing) + workspace resolution.
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	// Gate 2: editor minimum role.
	if !requireMinRole(w, r, "editor") {
		return
	}
	// Gate 3: full workspace visibility (visibleCollectionIDs == nil).
	if !s.requireUnrestrictedAccess(w, r, workspaceID, "Parking a role lane requires unrestricted workspace access") {
		return
	}
	// Gate 4: role lookup (same 404 as existing agent-role routes).
	roleID := chi.URLParam(r, "roleID")
	role, err := s.store.GetAgentRole(workspaceID, roleID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if role == nil {
		writeError(w, http.StatusNotFound, "not_found", "Agent role not found")
		return
	}

	var req setRoleParkRequest
	if err := decodeStrictSetRoleParkRequest(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "Invalid park request body: "+err.Error())
		return
	}
	if req.ExpectedParkSeq != nil && *req.ExpectedParkSeq < 0 {
		writeError(w, http.StatusBadRequest, "validation_error", "expected_park_seq must be non-negative")
		return
	}

	present := 0
	if req.At != nil {
		present++
	}
	if req.Before != nil {
		present++
	}
	if req.After != nil {
		present++
	}
	if present != 1 {
		writeError(w, http.StatusBadRequest, "validation_error", `Body must specify exactly one of "at", "before", or "after"`)
		return
	}

	var spec store.ParkSpec
	switch {
	case req.At != nil:
		if *req.At != "top" {
			writeError(w, http.StatusBadRequest, "validation_error", `"at" must be "top"`)
			return
		}
		spec = store.ParkAtTop()
	case req.Before != nil:
		ref := strings.TrimSpace(*req.Before)
		if ref == "" {
			writeError(w, http.StatusBadRequest, "validation_error", `"before" must be a non-empty item ref or ID`)
			return
		}
		// Gate 5: anchor lookup (missing and hidden answer byte-identical 404s).
		anchor, err := s.store.ResolveItem(workspaceID, ref)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if anchor == nil {
			writeError(w, http.StatusNotFound, "not_found", "Item not found")
			return
		}
		if !s.requireItemVisible(w, r, workspaceID, anchor) {
			return
		}
		spec = store.ParkBefore(anchor.ID)
	case req.After != nil:
		ref := strings.TrimSpace(*req.After)
		if ref == "" {
			writeError(w, http.StatusBadRequest, "validation_error", `"after" must be a non-empty item ref or ID`)
			return
		}
		// Gate 5: anchor lookup (missing and hidden answer byte-identical 404s).
		anchor, err := s.store.ResolveItem(workspaceID, ref)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if anchor == nil {
			writeError(w, http.StatusNotFound, "not_found", "Item not found")
			return
		}
		if !s.requireItemVisible(w, r, workspaceID, anchor) {
			return
		}
		spec = store.ParkAfter(anchor.ID)
	}

	park, err := s.store.SetRolePark(workspaceID, role.ID, spec, req.ExpectedParkSeq, currentUserID(r))
	if err != nil {
		var conflict *store.ParkConflictError
		switch {
		case errors.As(err, &conflict):
			writeError2(w, http.StatusConflict, "park_conflict",
				"Role park state has changed; retry with the current park_seq",
				map[string]interface{}{"actual_park_seq": conflict.ActualParkSeq})
		case errors.Is(err, store.ErrRoleNotFound):
			writeError(w, http.StatusNotFound, "not_found", "Agent role not found")
		case errors.Is(err, store.ErrParkAnchorNotFound):
			writeError(w, http.StatusNotFound, "not_found", "Item not found")
		case errors.Is(err, store.ErrParkAnchorNotInQueue):
			// Gate 6: anchor not in this lane's queue.
			writeError(w, http.StatusUnprocessableEntity, "park_anchor_not_in_queue",
				"Anchor item is not in this role lane's queue")
		default:
			writeInternalError(w, err)
		}
		return
	}

	firstParkedRef, err := s.store.FirstParkedQueueRef(workspaceID, role.ID, park, nil, nil)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	s.publishHomeChanged(workspaceID, "", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"park": roleParkResponse{
			Mode:           park.Mode,
			SortOrder:      park.SortOrder,
			FirstParkedRef: firstParkedRef,
			SetBy:          park.SetBy,
			SetAt:          park.SetAt,
			ParkSeq:        park.ParkSeq,
		},
	})
}

// handleClearRolePark clears a role lane's park line (idempotent):
// DELETE /api/v1/workspaces/{slug}/agent-roles/{roleID}/park
func (s *Server) handleClearRolePark(w http.ResponseWriter, r *http.Request) {
	// Gates 1-4 match PUT .../park.
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	if !requireMinRole(w, r, "editor") {
		return
	}
	if !s.requireUnrestrictedAccess(w, r, workspaceID, "Parking a role lane requires unrestricted workspace access") {
		return
	}
	roleID := chi.URLParam(r, "roleID")
	role, err := s.store.GetAgentRole(workspaceID, roleID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if role == nil {
		writeError(w, http.StatusNotFound, "not_found", "Agent role not found")
		return
	}

	var expectedSeq *int
	var req clearRoleParkRequest
	present, err := decodeStrictClearRoleParkRequest(r, &req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "Invalid park clear body: "+err.Error())
		return
	}
	if present {
		if req.ExpectedParkSeq != nil && *req.ExpectedParkSeq < 0 {
			writeError(w, http.StatusBadRequest, "validation_error", "expected_park_seq must be non-negative")
			return
		}
		expectedSeq = req.ExpectedParkSeq
	}

	seq, err := s.store.ClearRolePark(workspaceID, role.ID, expectedSeq)
	if err != nil {
		var conflict *store.ParkConflictError
		switch {
		case errors.As(err, &conflict):
			writeError2(w, http.StatusConflict, "park_conflict",
				"Role park state has changed; retry with the current park_seq",
				map[string]interface{}{"actual_park_seq": conflict.ActualParkSeq})
		case errors.Is(err, store.ErrRoleNotFound):
			writeError(w, http.StatusNotFound, "not_found", "Agent role not found")
		default:
			writeInternalError(w, err)
		}
		return
	}

	s.publishHomeChanged(workspaceID, "", "")
	writeJSON(w, http.StatusOK, map[string]any{
		"park":     nil,
		"park_seq": seq,
	})
}

// parseStrictBoolQueryParam parses an optional boolean query parameter strictly:
// absent -> (false, true); "true" -> (true, true); "false" -> (false, true);
// repeated, empty, or any other value -> (false, false).
func parseStrictBoolQueryParam(r *http.Request, name string) (bool, bool) {
	q := r.URL.Query()
	if !q.Has(name) {
		return false, true
	}
	vals := q[name]
	if len(vals) != 1 {
		return false, false
	}
	switch vals[0] {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// checkItemParkedForClaim refuses claiming a parked queue item with 409
// item_parked. Now items (live lease or active status) and non-queue items are
// never refused. The 409 body's first_parked_ref is computed from the caller's
// visible items only (null if none visible).
func (s *Server) checkItemParkedForClaim(w http.ResponseWriter, r *http.Request, workspaceID string, item *models.Item) bool {
	if item == nil || item.AgentRoleID == nil || *item.AgentRoleID == "" {
		return true
	}
	role, err := s.store.GetAgentRole(workspaceID, *item.AgentRoleID)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if role == nil || role.Park == nil {
		return true
	}
	inQueue, err := s.store.IsItemInRoleQueue(workspaceID, item)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if !inQueue {
		return true
	}
	if !store.ItemParked(role.Park, item.RoleSortOrder) {
		return true
	}

	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return false
	}
	rbCollIDs := visibleIDs
	var rbItemIDs []string
	rbFullCollIDs, rbGrantedItemIDs, rbGrantErr := s.guestResourceFilter(r, workspaceID)
	if rbGrantErr != nil {
		writeInternalError(w, rbGrantErr)
		return false
	}
	if len(rbGrantedItemIDs) > 0 {
		rbCollIDs = rbFullCollIDs
		rbItemIDs = rbGrantedItemIDs
	}

	firstParkedRef, err := s.store.FirstParkedQueueRef(workspaceID, role.ID, role.Park, rbCollIDs, rbItemIDs)
	if err != nil {
		writeInternalError(w, err)
		return false
	}

	writeError2(w, http.StatusConflict, "item_parked",
		fmt.Sprintf("Role %q is parked; pass ignore_park=true to claim anyway", role.Slug),
		map[string]interface{}{
			"role":             role.Slug,
			"first_parked_ref": firstParkedRef,
			"set_by":           role.Park.SetBy,
		})
	return false
}
