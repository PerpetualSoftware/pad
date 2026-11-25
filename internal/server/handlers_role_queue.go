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

// parseRoleQueueLimit parses the optional ?limit=N query parameter strictly:
// absent -> defaultRoleQueueLimit (5); single integer in 1..50 -> N; anything
// else (repeated, empty, non-integer, out of range) -> ok=false.
func parseRoleQueueLimit(r *http.Request) (int, bool) {
	q := r.URL.Query()
	if !q.Has("limit") {
		return defaultRoleQueueLimit, true
	}
	vals := q["limit"]
	if len(vals) != 1 {
		return 0, false
	}
	n, err := strconv.Atoi(vals[0])
	if err != nil || n < 1 || n > maxRoleQueueLimit {
		return 0, false
	}
	return n, true
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
	var nowEntries []store.RoleLaneEntry
	var queueEntries []store.RoleLaneEntry
	for _, entry := range lane {
		if !isCollectionVisible(entry.Item.CollectionID, visibleIDs) {
			continue
		}
		if !s.isItemVisibleToGuest(r, workspaceID, &entry.Item, fullCollIDs, grantedItemIDs) {
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

	// Self-scoped session presence for caller's own leased Now items only.
	user := currentUser(r)
	var connectedWorkers map[string]bool
	var busyWorkers map[string]bool
	presenceReady := false
	busyReady := false
	if user != nil && s.sessionPresence != nil {
		needPresence := false
		for _, e := range nowEntries {
			if _, hasLease := leases[e.Item.ID]; hasLease && e.LeaseUserID == user.ID && e.LeaseWorkerID != "" {
				needPresence = true
				break
			}
		}
		if needPresence {
			if sessions, pErr := s.sessionPresence.ListForUser(user.ID); pErr == nil {
				presenceReady = true
				connectedWorkers = make(map[string]bool, len(sessions))
				workerList := make([]string, 0, len(sessions))
				for _, sess := range sessions {
					if sess.WorkerID != "" {
						connectedWorkers[sess.WorkerID] = true
						workerList = append(workerList, sess.WorkerID)
					}
				}
				if busy, bErr := s.sessionPresence.BusyWorkers(user.ID, workerList); bErr == nil {
					busyReady = true
					busyWorkers = busy
				}
			}
		}
	}

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
			if presenceReady && user != nil && e.LeaseUserID == user.ID && e.LeaseWorkerID != "" {
				if !connectedWorkers[e.LeaseWorkerID] {
					live = "offline"
				} else if busyReady {
					if busyWorkers[e.LeaseWorkerID] {
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
		canEdit := false
		if appWriteAllows(r, e.Item.CollectionID) {
			if okEdit, editErr := s.canEditInCollection(r, workspaceID, e.Item.ID, e.Item.CollectionID); editErr == nil {
				canEdit = okEdit
			}
		}
		queueItems = append(queueItems, models.RoleQueueItem{
			Ref:           ref,
			Title:         e.Item.Title,
			Status:        extractFieldValue(e.Item.Fields, doneKey),
			Priority:      extractFieldValue(e.Item.Fields, "priority"),
			RoleSortOrder: e.Item.RoleSortOrder,
			Blocked:       blocked,
			Ready:         !parked && !blocked,
			CanEdit:       canEdit,
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

	// Capability checks: match lane reorder handler (editor role) and park handler (editor + unrestricted).
	canReorder := requireRole(r, "editor")
	canPark := canReorder && visibleIDs == nil

	writeJSON(w, http.StatusOK, models.RoleQueueResponse{
		Workspace: ws.Slug,
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
	})
}
