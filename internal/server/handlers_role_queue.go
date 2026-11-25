package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

const (
	defaultRoleQueueLimit = 5
	maxRoleQueueLimit     = 50
)

// parseStrictBoundedIntQueryParam parses an optional integer query parameter
// strictly: absent -> (def, true); single integer in [minVal, maxVal] -> (n, true);
// anything else (repeated, empty, non-integer, out of range) -> (0, false).
func parseStrictBoundedIntQueryParam(r *http.Request, name string, def, minVal, maxVal int) (int, bool) {
	q := r.URL.Query()
	if !q.Has(name) {
		return def, true
	}
	vals := q[name]
	if len(vals) != 1 {
		return 0, false
	}
	n, err := strconv.Atoi(vals[0])
	if err != nil || n < minVal || n > maxVal {
		return 0, false
	}
	return n, true
}

// parseRoleQueueLimit parses the optional ?limit=N query parameter strictly:
// absent -> defaultRoleQueueLimit (5); single integer in 1..50 -> N; anything
// else (repeated, empty, non-integer, out of range) -> ok=false.
func parseRoleQueueLimit(r *http.Request) (int, bool) {
	return parseStrictBoundedIntQueryParam(r, "limit", defaultRoleQueueLimit, 1, maxRoleQueueLimit)
}

type sessionPresenceSnapshot struct {
	ready     bool
	busyReady bool
	connected map[string]bool
	busy      map[string]bool
}

func laneNeedsOwnPresence(nowEntries []store.RoleLaneEntry, leases map[string]models.ItemLease, userID string) bool {
	if userID == "" {
		return false
	}
	for _, e := range nowEntries {
		if _, hasLease := leases[e.Item.ID]; hasLease && e.LeaseUserID == userID && e.LeaseWorkerID != "" {
			return true
		}
	}
	return false
}

func (s *Server) loadOwnSessionPresence(userID string) sessionPresenceSnapshot {
	var snap sessionPresenceSnapshot
	if userID == "" || s.sessionPresence == nil {
		return snap
	}
	sessions, err := s.sessionPresence.ListForUser(userID)
	if err != nil {
		return snap
	}
	snap.ready = true
	snap.connected = make(map[string]bool, len(sessions))
	workerList := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		if sess.WorkerID != "" {
			snap.connected[sess.WorkerID] = true
			workerList = append(workerList, sess.WorkerID)
		}
	}
	if busy, bErr := s.sessionPresence.BusyWorkers(userID, workerList); bErr == nil {
		snap.busyReady = true
		snap.busy = busy
	}
	return snap
}

func partitionRoleLane(lane []store.RoleLaneEntry, visible func(collectionID, itemID string) bool) (nowEntries, queueEntries []store.RoleLaneEntry) {
	for _, entry := range lane {
		if !visible(entry.Item.CollectionID, entry.Item.ID) {
			continue
		}
		// The lane is items in collections that track work; an active item in
		// any other collection is not this role's Now work.
		if !entry.TracksWork {
			continue
		}
		if entry.IsNow {
			nowEntries = append(nowEntries, entry)
		} else if entry.InQueue {
			queueEntries = append(queueEntries, entry)
		}
	}
	return nowEntries, queueEntries
}

func buildRoleQueueLaneResponse(
	wsSlug string,
	role *models.AgentRole,
	nowEntries, queueEntries []store.RoleLaneEntry,
	leases map[string]models.ItemLease,
	firstActiveBlocker map[string]activeBlocker,
	limit int,
	callerUserID string,
	presence sessionPresenceSnapshot,
	canReorder, canPark bool,
	canEdit func(itemID, collectionID string) bool,
) models.RoleQueueResponse {
	nowItems := make([]models.RoleQueueNowItem, 0, len(nowEntries))
	for _, e := range nowEntries {
		ref := e.Item.Ref
		if ref == "" {
			ref = e.Item.ID
		}
		doneKey := e.DoneKey
		if doneKey == "" {
			doneKey = "status"
		}
		since := e.Item.UpdatedAt
		var leaseOut *models.RoleQueueLease
		live := "unknown"
		if lease, hasLease := leases[e.Item.ID]; hasLease {
			since = lease.AcquiredAt
			leaseOut = &models.RoleQueueLease{
				Holder:    lease.Holder,
				ExpiresAt: lease.ExpiresAt,
			}
			if presence.ready && callerUserID != "" && e.LeaseUserID == callerUserID && e.LeaseWorkerID != "" {
				if !presence.connected[e.LeaseWorkerID] {
					live = "offline"
				} else if presence.busyReady {
					if presence.busy[e.LeaseWorkerID] {
						live = "busy"
					} else {
						live = "idle"
					}
				}
			}
		}
		nowItems = append(nowItems, models.RoleQueueNowItem{
			Ref:    ref,
			Title:  e.Item.Title,
			Status: extractFieldValue(e.Item.Fields, doneKey),
			Since:  since,
			Lease:  leaseOut,
			Live:   live,
		})
	}

	// Compute visible queue counts and park position across the FULL visible queue.
	queueTotal := len(queueEntries)
	parkedTotal := 0
	var firstParkedIdx *int
	var firstParkedRef *string
	for i, e := range queueEntries {
		if store.ItemParked(role.Park, e.Item.RoleSortOrder) {
			parkedTotal++
			if firstParkedIdx == nil {
				idx := i
				firstParkedIdx = &idx
				ref := e.Item.Ref
				if ref == "" {
					ref = e.Item.ID
				}
				firstParkedRef = &ref
			}
		}
	}

	// Build the returned prefix (up to limit).
	retCount := queueTotal
	if retCount > limit {
		retCount = limit
	}
	queueItems := make([]models.RoleQueueItem, 0, retCount)
	for i := 0; i < retCount; i++ {
		e := queueEntries[i]
		ref := e.Item.Ref
		if ref == "" {
			ref = e.Item.ID
		}
		doneKey := e.DoneKey
		if doneKey == "" {
			doneKey = "status"
		}
		_, blocked := firstActiveBlocker[e.Item.ID]
		parked := store.ItemParked(role.Park, e.Item.RoleSortOrder)
		queueItems = append(queueItems, models.RoleQueueItem{
			Ref:           ref,
			Title:         e.Item.Title,
			Status:        extractFieldValue(e.Item.Fields, doneKey),
			Priority:      extractFieldValue(e.Item.Fields, "priority"),
			RoleSortOrder: e.Item.RoleSortOrder,
			Blocked:       blocked,
			Ready:         !parked && !blocked,
			CanEdit:       canEdit(e.Item.ID, e.Item.CollectionID),
		})
	}

	var parkOut *models.RoleQueuePark
	if role.Park != nil {
		parkOut = &models.RoleQueuePark{
			Mode:           role.Park.Mode,
			Index:          firstParkedIdx,
			FirstParkedRef: firstParkedRef,
			SetBy:          role.Park.SetBy,
			SetAt:          role.Park.SetAt,
			ParkSeq:        role.Park.ParkSeq,
		}
	}

	return models.RoleQueueResponse{
		Workspace: wsSlug,
		Role: models.RoleQueueRole{
			Slug: role.Slug,
			Name: role.Name,
			Icon: role.Icon,
		},
		CanReorder:  canReorder,
		CanPark:     canPark,
		Now:         nowItems,
		Queue:       queueItems,
		Park:        parkOut,
		QueueTotal:  queueTotal,
		ParkedTotal: parkedTotal,
		QueueMore:   queueTotal > len(queueItems),
	}
}

// handleGetRoleQueue returns the caller's visible Now items, canonical Queue
// prefix, park status, and capabilities for a single agent role lane:
// GET /api/v1/workspaces/{slug}/agent-roles/{roleID}/queue?limit=N
func (s *Server) handleGetRoleQueue(w http.ResponseWriter, r *http.Request) {
	// Gate 1: workspace middleware + workspace resolution.
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	ws, err := s.store.GetWorkspaceByID(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if ws == nil {
		writeError(w, http.StatusNotFound, "not_found", "Workspace not found")
		return
	}

	// Gate 2: role lookup (id or slug).
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

	// Gate 3: limit validation.
	limit, validLimit := parseRoleQueueLimit(r)
	if !validLimit {
		writeError(w, http.StatusBadRequest, "validation_error", "limit must be an integer between 1 and 50")
		return
	}

	// Resolve caller's visibility once.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	fullCollIDs, grantedItemIDs, err := s.guestResourceFilter(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Load lane once, live leases once, collections/blockers once.
	lane, err := s.store.LoadRoleLane(workspaceID, role.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	leases, err := s.store.ListItemLeases(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	colls, err := s.store.ListCollections(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	ctxMap := buildDoneContextMap(colls)
	firstActiveBlocker, err := s.firstActiveBlockers(r, workspaceID, visibleIDs, fullCollIDs, grantedItemIDs, ctxMap)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Partition visible lane entries into Now and Queue.
	nowEntries, queueEntries := partitionRoleLane(lane, func(collectionID, itemID string) bool {
		if !isCollectionVisible(collectionID, visibleIDs) {
			return false
		}
		return s.isItemVisibleToGuest(r, workspaceID, &models.Item{ID: itemID, CollectionID: collectionID}, fullCollIDs, grantedItemIDs)
	})

	// Self-scoped session presence for caller's own leased Now items only.
	user := currentUser(r)
	callerUserID := ""
	if user != nil {
		callerUserID = user.ID
	}
	var presence sessionPresenceSnapshot
	if laneNeedsOwnPresence(nowEntries, leases, callerUserID) {
		presence = s.loadOwnSessionPresence(callerUserID)
	}

	// Capability checks: match lane reorder handler (editor role) and park handler (editor + unrestricted).
	canReorder := requireRole(r, "editor")
	canPark := canReorder && visibleIDs == nil

	resp := buildRoleQueueLaneResponse(
		ws.Slug,
		role,
		nowEntries,
		queueEntries,
		leases,
		firstActiveBlocker,
		limit,
		callerUserID,
		presence,
		canReorder,
		canPark,
		func(itemID, collectionID string) bool {
			if appWriteAllows(r, collectionID) {
				if okEdit, editErr := s.canEditInCollection(r, workspaceID, itemID, collectionID); editErr == nil {
					return okEdit
				}
			}
			return false
		},
	)
	writeJSON(w, http.StatusOK, resp)
}
