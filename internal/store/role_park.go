package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const (
	// roleSortStep is the spacing between adjacent items when a role lane is
	// respaced or when a line/arrival is placed past the bottom of a lane.
	roleSortStep = 1024

	// roleSortRespaceLimit caps role_sort_order and park_sort_order well below
	// Postgres int4 max (2^31 - 1) so headroom never runs out.
	roleSortRespaceLimit = 1 << 30
)

var (
	// ErrParkConflict reports that expected_park_seq did not match the role's
	// current park_seq. Unwraps from *ParkConflictError.
	ErrParkConflict = errors.New("role park sequence conflict")

	// ErrParkAnchorNotInQueue reports that the named anchor item exists in the
	// workspace but is not in this role lane's queue (wrong role, terminal,
	// Now item, or reference collection).
	ErrParkAnchorNotInQueue = errors.New("park anchor item is not in this role's queue")

	// ErrParkAnchorNotFound reports that the named anchor item does not exist
	// as a live item in the workspace.
	ErrParkAnchorNotFound = errors.New("park anchor item not found")

	// ErrRoleNotFound reports that no agent role matched the workspace + id/slug.
	ErrRoleNotFound = errors.New("agent role not found")

	// ErrInvalidParkSpec reports a malformed ParkSpec (zero or multiple targets).
	ErrInvalidParkSpec = errors.New("invalid park spec: specify exactly one of at=top, before, or after")
)

// ParkConflictError is returned when expected_park_seq does not match the
// role's current park_seq.
type ParkConflictError struct {
	ActualParkSeq int
}

func (e *ParkConflictError) Error() string {
	return fmt.Sprintf("role park sequence conflict: actual_park_seq is %d", e.ActualParkSeq)
}

func (e *ParkConflictError) Unwrap() error {
	return ErrParkConflict
}

// ParkSpec names where to place a role's park line: exactly one of AtTop,
// BeforeItemID, or AfterItemID must be set.
type ParkSpec struct {
	AtTop        bool
	BeforeItemID string
	AfterItemID  string
}

// ParkAtTop returns a ParkSpec that parks the entire queue from the top.
func ParkAtTop() ParkSpec { return ParkSpec{AtTop: true} }

// ParkBefore returns a ParkSpec that places the park line immediately before itemID.
func ParkBefore(itemID string) ParkSpec { return ParkSpec{BeforeItemID: itemID} }

// ParkAfter returns a ParkSpec that places the park line immediately after itemID.
func ParkAfter(itemID string) ParkSpec { return ParkSpec{AfterItemID: itemID} }

func (p ParkSpec) validate() error {
	n := 0
	if p.AtTop {
		n++
	}
	if p.BeforeItemID != "" {
		n++
	}
	if p.AfterItemID != "" {
		n++
	}
	if n != 1 {
		return ErrInvalidParkSpec
	}
	return nil
}

// roleLaneLess defines the canonical lane ordering:
// (role_sort_order asc, created_at asc, item_number asc).
func roleLaneLess(a, b models.Item) bool {
	if a.RoleSortOrder != b.RoleSortOrder {
		return a.RoleSortOrder < b.RoleSortOrder
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return itemNumberOrZero(a.ItemNumber) < itemNumberOrZero(b.ItemNumber)
}

func itemNumberOrZero(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// SortRoleLane sorts items in canonical role lane order using sort.SliceStable:
// role_sort_order asc, created_at asc, item_number asc.
func SortRoleLane(items []models.Item) {
	sort.SliceStable(items, func(i, j int) bool {
		return roleLaneLess(items[i], items[j])
	})
}

// ItemParked is the shared predicate reporting whether a queue item at
// roleSortOrder is parked under park. Now items are never parked; callers
// should only pass queue items (or check IsItemNow first).
func ItemParked(park *models.RolePark, roleSortOrder int) bool {
	if park == nil {
		return false
	}
	switch park.Mode {
	case models.ParkModeTop:
		return true
	case models.ParkModeLine:
		return park.SortOrder != nil && roleSortOrder >= *park.SortOrder
	default:
		return false
	}
}

// IsItemNow reports whether item is actively being worked on: either it holds
// a live execution lease or its done-field status passes models.IsActiveStatus.
func IsItemNow(item *models.Item, doneFieldKey string, hasLiveLease bool) bool {
	if hasLiveLease {
		return true
	}
	if item == nil {
		return false
	}
	if doneFieldKey == "" {
		doneFieldKey = "status"
	}
	return models.IsActiveStatus(extractFieldValue(item.Fields, doneFieldKey))
}

func buildRolePark(mode string, sortOrder sql.NullInt64, setAt, setByUserID, setBy sql.NullString, parkSeq int) *models.RolePark {
	if mode == "" || mode == models.ParkModeNone {
		return nil
	}
	p := &models.RolePark{
		Mode:    mode,
		ParkSeq: parkSeq,
	}
	if sortOrder.Valid {
		v := int(sortOrder.Int64)
		p.SortOrder = &v
	}
	if setAt.Valid && setAt.String != "" {
		t := parseTime(setAt.String)
		p.SetAt = &t
	}
	if setByUserID.Valid && setByUserID.String != "" {
		uid := setByUserID.String
		p.SetByUserID = &uid
	}
	if setBy.Valid && setBy.String != "" {
		name := setBy.String
		p.SetBy = &name
	}
	return p
}

type roleLaneCollectionInfo struct {
	schema     models.CollectionSchema
	settings   models.CollectionSettings
	prefix     string
	doneKey    string
	tracksWork bool
}

type roleLaneEntry struct {
	item    models.Item
	inQueue bool
}

func (s *Store) loadRoleLaneCollectionsQ(q Queryer, workspaceID string) (map[string]roleLaneCollectionInfo, error) {
	rows, err := q.Query(s.q(`
		SELECT id, schema, settings, prefix
		FROM collections
		WHERE workspace_id = ? AND deleted_at IS NULL
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load role lane collections: %w", err)
	}
	defer rows.Close()

	out := make(map[string]roleLaneCollectionInfo)
	for rows.Next() {
		var id, rawSchema, rawSettings, prefix string
		if err := rows.Scan(&id, &rawSchema, &rawSettings, &prefix); err != nil {
			return nil, err
		}
		var info roleLaneCollectionInfo
		info.prefix = prefix
		if rawSchema != "" {
			if err := models.UnmarshalItemFieldSchema([]byte(rawSchema), &info.schema); err != nil {
				info.schema = models.CollectionSchema{}
			}
		}
		if rawSettings != "" {
			_ = json.Unmarshal([]byte(rawSettings), &info.settings)
		}
		info.doneKey = models.DoneFieldKey(info.schema, info.settings)
		info.tracksWork = models.CollectionTracksWork(info.settings)
		out[id] = info
	}
	return out, rows.Err()
}

// loadRoleLaneQ returns the canonical full lane for (workspaceID, roleID): all
// live non-terminal items of the role in live collections, sorted with
// roleLaneLess (sort.SliceStable). Each entry records whether it belongs to
// the queue (tracks_work collection AND not a Now item).
func (s *Store) loadRoleLaneQ(q Queryer, workspaceID, roleID, nowStr string) ([]roleLaneEntry, error) {
	colls, err := s.loadRoleLaneCollectionsQ(q, workspaceID)
	if err != nil {
		return nil, err
	}

	rows, err := q.Query(s.q(`
		SELECT i.id, i.collection_id, i.fields, i.role_sort_order, i.created_at, i.item_number,
		       i.lease_holder, i.lease_expires_at
		FROM items i
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL
		WHERE i.workspace_id = ? AND i.agent_role_id = ? AND i.deleted_at IS NULL
	`), workspaceID, roleID)
	if err != nil {
		return nil, fmt.Errorf("load role lane items: %w", err)
	}
	defer rows.Close()

	var entries []roleLaneEntry
	for rows.Next() {
		var it models.Item
		var createdAt string
		var itemNum sql.NullInt64
		var leaseHolder, leaseExpiresAt sql.NullString
		if err := rows.Scan(
			&it.ID, &it.CollectionID, &it.Fields, &it.RoleSortOrder, &createdAt, &itemNum,
			&leaseHolder, &leaseExpiresAt,
		); err != nil {
			return nil, err
		}
		it.CreatedAt = parseTime(createdAt)
		if itemNum.Valid {
			n := int(itemNum.Int64)
			it.ItemNumber = &n
		}

		cinfo, ok := colls[it.CollectionID]
		if !ok {
			continue
		}
		if cinfo.prefix != "" && it.ItemNumber != nil {
			it.Ref = cinfo.prefix + "-" + strconv.Itoa(*it.ItemNumber)
		}

		var fieldsMap map[string]any
		if it.Fields != "" && it.Fields != "{}" {
			_ = json.Unmarshal([]byte(it.Fields), &fieldsMap)
		}
		if models.IsTerminalItem(fieldsMap, cinfo.schema, cinfo.settings) {
			continue
		}

		hasLiveLease := leaseHolder.Valid && leaseExpiresAt.Valid && leaseExpiresAt.String > nowStr
		isNow := IsItemNow(&it, cinfo.doneKey, hasLiveLease)
		entries = append(entries, roleLaneEntry{
			item:    it,
			inQueue: cinfo.tracksWork && !isNow,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return roleLaneLess(entries[i].item, entries[j].item)
	})
	return entries, nil
}

// respaceRoleLane rewrites every live non-terminal item in the role's
// canonical lane (Now items included) to role_sort_order = 1024*(i+1), bumping
// workspace seq per changed row, and remaps park_sort_order when mode is line
// so the exact same items stay above and below the park line.
// Caller must already hold the workspace seq lock and the role row lock on tx.
func (s *Store) respaceRoleLane(tx *sql.Tx, workspaceID, roleID string) ([]roleLaneEntry, error) {
	nowStr := now()
	lane, err := s.loadRoleLaneQ(tx, workspaceID, roleID, nowStr)
	if err != nil {
		return nil, err
	}

	var parkMode string
	var parkSortOrder sql.NullInt64
	if err := tx.QueryRow(s.q(`
		SELECT park_mode, park_sort_order
		FROM agent_roles
		WHERE id = ? AND workspace_id = ?
	`), roleID, workspaceID).Scan(&parkMode, &parkSortOrder); err != nil {
		return nil, fmt.Errorf("respace read role park: %w", err)
	}

	oldOrders := make([]int, len(lane))
	for i := range lane {
		oldOrders[i] = lane[i].item.RoleSortOrder
	}

	stmt, err := tx.Prepare(s.q("UPDATE items SET role_sort_order = ?, seq = " + nextWorkspaceSeqSubquery + " WHERE id = ? AND workspace_id = ? AND deleted_at IS NULL"))
	if err != nil {
		return nil, fmt.Errorf("respace prepare update: %w", err)
	}
	defer stmt.Close()

	for i := range lane {
		newOrder := roleSortStep * (i + 1)
		if lane[i].item.RoleSortOrder != newOrder {
			if _, err := stmt.Exec(newOrder, workspaceID, lane[i].item.ID, workspaceID); err != nil {
				return nil, fmt.Errorf("respace update item %s: %w", lane[i].item.ID, err)
			}
			lane[i].item.RoleSortOrder = newOrder
		}
	}

	if parkMode == models.ParkModeLine && parkSortOrder.Valid {
		oldP := int(parkSortOrder.Int64)
		newP := roleSortStep * (len(lane) + 1)
		for i, oldOrd := range oldOrders {
			if oldOrd >= oldP {
				newP = roleSortStep * (i + 1)
				break
			}
		}
		if newP != oldP {
			if _, err := tx.Exec(s.q(`UPDATE agent_roles SET park_sort_order = ? WHERE id = ?`), newP, roleID); err != nil {
				return nil, fmt.Errorf("respace remap park_sort_order: %w", err)
			}
		}
	}

	return lane, nil
}

// nextRoleLaneArrivalSortOrderTx computes the role_sort_order for an item
// arriving in roleIDOrSlug's lane: max(lane max role_sort_order, P if mode
// line else 0) + 1024. If that value would pass 2^30, the lane is respaced
// first (which also remaps P).
// Caller must already hold the workspace seq lock on tx.
func (s *Store) nextRoleLaneArrivalSortOrderTx(tx *sql.Tx, workspaceID, roleIDOrSlug string) (int, error) {
	var resolvedRoleID, parkMode string
	var parkSortOrder sql.NullInt64
	err := tx.QueryRow(s.q(`
		SELECT id, park_mode, park_sort_order
		FROM agent_roles
		WHERE workspace_id = ? AND (id = ? OR slug = ?)`+s.dialect.ForUpdate()),
		workspaceID, roleIDOrSlug, roleIDOrSlug,
	).Scan(&resolvedRoleID, &parkMode, &parkSortOrder)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("role arrival lock role: %w", err)
	}

	nowStr := now()
	lane, err := s.loadRoleLaneQ(tx, workspaceID, resolvedRoleID, nowStr)
	if err != nil {
		return 0, err
	}

	calcNext := func(entries []roleLaneEntry, mode string, p sql.NullInt64) int64 {
		base := int64(0)
		for _, e := range entries {
			if int64(e.item.RoleSortOrder) > base {
				base = int64(e.item.RoleSortOrder)
			}
		}
		if mode == models.ParkModeLine && p.Valid && p.Int64 > base {
			base = p.Int64
		}
		return base + roleSortStep
	}

	cand := calcNext(lane, parkMode, parkSortOrder)
	if cand > roleSortRespaceLimit {
		lane, err = s.respaceRoleLane(tx, workspaceID, resolvedRoleID)
		if err != nil {
			return 0, err
		}
		if err := tx.QueryRow(s.q(`
			SELECT park_mode, park_sort_order
			FROM agent_roles
			WHERE id = ? AND workspace_id = ?
		`), resolvedRoleID, workspaceID).Scan(&parkMode, &parkSortOrder); err != nil {
			return 0, fmt.Errorf("role arrival reload park after respace: %w", err)
		}
		cand = calcNext(lane, parkMode, parkSortOrder)
	}
	return int(cand), nil
}

// SetRolePark places or moves the park line for a role lane.
// Lock order: workspace seq lock -> agent_roles row (FOR UPDATE on Postgres) -> items.
func (s *Store) SetRolePark(workspaceID, roleIDOrSlug string, spec ParkSpec, expectedSeq *int, userID string) (*models.RolePark, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := s.acquireWorkspaceSeqLock(tx, workspaceID); err != nil {
		return nil, err
	}

	var resolvedRoleID string
	var currentSeq int
	err = tx.QueryRow(s.q(`
		SELECT id, park_seq
		FROM agent_roles
		WHERE workspace_id = ? AND (id = ? OR slug = ?)`+s.dialect.ForUpdate()),
		workspaceID, roleIDOrSlug, roleIDOrSlug,
	).Scan(&resolvedRoleID, &currentSeq)
	if err == sql.ErrNoRows {
		return nil, ErrRoleNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("set role park lock role: %w", err)
	}

	if expectedSeq != nil && *expectedSeq != currentSeq {
		return nil, &ParkConflictError{ActualParkSeq: currentSeq}
	}

	nowStr := now()
	var mode string
	var parkOrderArg any

	if spec.AtTop {
		mode = models.ParkModeTop
		parkOrderArg = nil
	} else {
		lane, err := s.loadRoleLaneQ(tx, workspaceID, resolvedRoleID, nowStr)
		if err != nil {
			return nil, err
		}

		anchorID := spec.BeforeItemID
		if anchorID == "" {
			anchorID = spec.AfterItemID
		}

		// Build queue index mapping into lane.
		var queueLaneIndices []int
		anchorQueueIdx := -1
		for i, entry := range lane {
			if entry.inQueue {
				if entry.item.ID == anchorID {
					anchorQueueIdx = len(queueLaneIndices)
				}
				queueLaneIndices = append(queueLaneIndices, i)
			}
		}

		if anchorQueueIdx < 0 {
			// Distinguish missing item from item not in this lane's queue.
			var exists int
			qErr := tx.QueryRow(s.q(`
				SELECT 1
				FROM items i
				JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL
				WHERE i.id = ? AND i.workspace_id = ? AND i.deleted_at IS NULL
			`), anchorID, workspaceID).Scan(&exists)
			if qErr == sql.ErrNoRows {
				return nil, ErrParkAnchorNotFound
			}
			if qErr != nil {
				return nil, fmt.Errorf("check anchor item: %w", qErr)
			}
			return nil, ErrParkAnchorNotInQueue
		}

		mode = models.ParkModeLine
		if spec.BeforeItemID != "" {
			k := queueLaneIndices[anchorQueueIdx]
			if (k > 0 && lane[k-1].item.RoleSortOrder == lane[k].item.RoleSortOrder) || lane[k].item.RoleSortOrder > roleSortRespaceLimit {
				lane, err = s.respaceRoleLane(tx, workspaceID, resolvedRoleID)
				if err != nil {
					return nil, err
				}
			}
			parkOrderArg = lane[k].item.RoleSortOrder
		} else {
			// After(X): equivalent to Before(successor in queue); if X is last, X.role_sort_order + 1024.
			if anchorQueueIdx+1 < len(queueLaneIndices) {
				k := queueLaneIndices[anchorQueueIdx+1]
				if (k > 0 && lane[k-1].item.RoleSortOrder == lane[k].item.RoleSortOrder) || lane[k].item.RoleSortOrder > roleSortRespaceLimit {
					lane, err = s.respaceRoleLane(tx, workspaceID, resolvedRoleID)
					if err != nil {
						return nil, err
					}
				}
				parkOrderArg = lane[k].item.RoleSortOrder
			} else {
				k := queueLaneIndices[anchorQueueIdx]
				cand := int64(lane[k].item.RoleSortOrder) + roleSortStep
				if cand > roleSortRespaceLimit {
					lane, err = s.respaceRoleLane(tx, workspaceID, resolvedRoleID)
					if err != nil {
						return nil, err
					}
					cand = int64(lane[k].item.RoleSortOrder) + roleSortStep
				}
				parkOrderArg = int(cand)
			}
		}
	}

	var userArg any
	if userID != "" {
		userArg = userID
	}

	if _, err := tx.Exec(s.q(`
		UPDATE agent_roles
		SET park_mode = ?, park_sort_order = ?, park_set_at = ?, park_set_by_user_id = ?, park_seq = park_seq + 1
		WHERE id = ? AND workspace_id = ?
	`), mode, parkOrderArg, nowStr, userArg, resolvedRoleID, workspaceID); err != nil {
		return nil, fmt.Errorf("update role park: %w", err)
	}

	var gotMode string
	var gotSortOrder sql.NullInt64
	var gotSetAt, gotSetByUserID, gotSetBy sql.NullString
	var gotSeq int
	if err := tx.QueryRow(s.q(`
		SELECT r.park_mode, r.park_sort_order, r.park_set_at, r.park_set_by_user_id, r.park_seq,
		       (SELECT CASE WHEN u.name <> '' THEN u.name ELSE u.email END FROM users u WHERE u.id = r.park_set_by_user_id)
		FROM agent_roles r
		WHERE r.id = ? AND r.workspace_id = ?
	`), resolvedRoleID, workspaceID).Scan(&gotMode, &gotSortOrder, &gotSetAt, &gotSetByUserID, &gotSeq, &gotSetBy); err != nil {
		return nil, fmt.Errorf("read updated role park: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return buildRolePark(gotMode, gotSortOrder, gotSetAt, gotSetByUserID, gotSetBy, gotSeq), nil
}

// ClearRolePark clears a role's park line (mode "none", park_sort_order NULL).
// Idempotent when already "none": returns the current park_seq without bumping.
// Lock order: workspace seq lock -> agent_roles row (FOR UPDATE on Postgres).
func (s *Store) ClearRolePark(workspaceID, roleIDOrSlug string, expectedSeq *int) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if err := s.acquireWorkspaceSeqLock(tx, workspaceID); err != nil {
		return 0, err
	}

	var resolvedRoleID, currentMode string
	var currentSeq int
	err = tx.QueryRow(s.q(`
		SELECT id, park_mode, park_seq
		FROM agent_roles
		WHERE workspace_id = ? AND (id = ? OR slug = ?)`+s.dialect.ForUpdate()),
		workspaceID, roleIDOrSlug, roleIDOrSlug,
	).Scan(&resolvedRoleID, &currentMode, &currentSeq)
	if err == sql.ErrNoRows {
		return 0, ErrRoleNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("clear role park lock role: %w", err)
	}

	if expectedSeq != nil && *expectedSeq != currentSeq {
		return currentSeq, &ParkConflictError{ActualParkSeq: currentSeq}
	}

	if currentMode == models.ParkModeNone {
		return currentSeq, nil
	}

	newSeq := currentSeq + 1
	if _, err := tx.Exec(s.q(`
		UPDATE agent_roles
		SET park_mode = 'none', park_sort_order = NULL, park_set_at = NULL, park_set_by_user_id = NULL, park_seq = ?
		WHERE id = ? AND workspace_id = ?
	`), newSeq, resolvedRoleID, workspaceID); err != nil {
		return 0, fmt.Errorf("clear role park update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newSeq, nil
}

// FirstParkedQueueRef returns the Ref of the first parked queue item in the
// role lane (in canonical lane order), or nil when none exists.
// When collectionIDs is non-nil (or itemIDs is non-empty), only items visible
// under that filter are considered.
func (s *Store) FirstParkedQueueRef(workspaceID, roleID string, park *models.RolePark, collectionIDs, itemIDs []string) (*string, error) {
	if park == nil {
		return nil, nil
	}
	lane, err := s.loadRoleLaneQ(s.db, workspaceID, roleID, now())
	if err != nil {
		return nil, err
	}

	restricted := collectionIDs != nil || len(itemIDs) > 0
	var collSet, itemSet map[string]bool
	if restricted {
		collSet = make(map[string]bool, len(collectionIDs))
		for _, c := range collectionIDs {
			collSet[c] = true
		}
		itemSet = make(map[string]bool, len(itemIDs))
		for _, id := range itemIDs {
			itemSet[id] = true
		}
	}

	for _, entry := range lane {
		if !entry.inQueue {
			continue
		}
		if restricted && !collSet[entry.item.CollectionID] && !itemSet[entry.item.ID] {
			continue
		}
		if ItemParked(park, entry.item.RoleSortOrder) {
			ref := entry.item.Ref
			if ref == "" {
				ref = entry.item.ID
			}
			return &ref, nil
		}
	}
	return nil, nil
}

// IsItemInRoleQueue reports whether item is eligible to be parked in its role
// lane's queue: live non-terminal item in a tracks_work collection, and not a
// Now item (neither live execution lease nor active status).
func (s *Store) IsItemInRoleQueue(workspaceID string, item *models.Item) (bool, error) {
	if item == nil || item.AgentRoleID == nil || *item.AgentRoleID == "" {
		return false, nil
	}
	colls, err := s.loadRoleLaneCollectionsQ(s.db, workspaceID)
	if err != nil {
		return false, err
	}
	cinfo, ok := colls[item.CollectionID]
	if !ok || !cinfo.tracksWork {
		return false, nil
	}
	var fieldsMap map[string]any
	if item.Fields != "" && item.Fields != "{}" {
		_ = json.Unmarshal([]byte(item.Fields), &fieldsMap)
	}
	if models.IsTerminalItem(fieldsMap, cinfo.schema, cinfo.settings) {
		return false, nil
	}
	lease, err := s.GetItemLease(item.ID)
	if err != nil {
		return false, err
	}
	if IsItemNow(item, cinfo.doneKey, lease != nil) {
		return false, nil
	}
	return true, nil
}
