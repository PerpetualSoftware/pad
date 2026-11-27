package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

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
	schema      models.CollectionSchema
	settings    models.CollectionSettings
	rawSchema   string
	rawSettings string
	prefix      string
	doneKey     string
	tracksWork  bool
}

// RoleLaneCollectionInfo describes one live workspace collection loaded during
// LoadWorkspaceRoleLanesQ so callers can build follow-up queries without
// re-reading collections.
type RoleLaneCollectionInfo struct {
	ID          string
	Schema      models.CollectionSchema
	Settings    models.CollectionSettings
	RawSchema   string
	RawSettings string
	Prefix      string
	DoneKey     string
	TracksWork  bool
}

type roleLaneEntry struct {
	item          models.Item
	inQueue       bool
	isNow         bool
	doneKey       string
	tracksWork    bool
	leaseWorkerID string
	leaseUserID   string
	lease         *models.ItemLease
}

// RoleLaneEntry is one live, non-terminal item in a role's canonical lane.
// Returned by LoadRoleLane in canonical lane order (SortRoleLane).
type RoleLaneEntry struct {
	Item          models.Item
	InQueue       bool
	IsNow         bool
	DoneKey       string
	TracksWork    bool
	LeaseWorkerID string
	LeaseUserID   string
	Lease         *models.ItemLease
}

// toPublicRoleLaneEntry converts an internal roleLaneEntry into the exported RoleLaneEntry.
func toPublicRoleLaneEntry(e roleLaneEntry) RoleLaneEntry {
	return RoleLaneEntry{
		Item:          e.item,
		InQueue:       e.inQueue,
		IsNow:         e.isNow,
		DoneKey:       e.doneKey,
		TracksWork:    e.tracksWork,
		LeaseWorkerID: e.leaseWorkerID,
		LeaseUserID:   e.leaseUserID,
		Lease:         e.lease,
	}
}

// buildRoleLaneEntry converts raw scanned item/lease columns into a roleLaneEntry,
// returning ok=false when the item's collection is absent or the item is terminal.
func buildRoleLaneEntry(
	it models.Item,
	createdAt, updatedAt string,
	itemNum sql.NullInt64,
	leaseHolder, leaseWorkerID, leaseUserID, leaseAcquiredAt, leaseExpiresAt sql.NullString,
	colls map[string]roleLaneCollectionInfo,
	nowStr string,
) (roleLaneEntry, bool) {
	it.CreatedAt = parseTime(createdAt)
	it.UpdatedAt = parseTime(updatedAt)
	if itemNum.Valid {
		n := int(itemNum.Int64)
		it.ItemNumber = &n
	}

	cinfo, ok := colls[it.CollectionID]
	if !ok {
		return roleLaneEntry{}, false
	}
	if cinfo.prefix != "" && it.ItemNumber != nil {
		it.Ref = cinfo.prefix + "-" + strconv.Itoa(*it.ItemNumber)
	}

	var fieldsMap map[string]any
	if it.Fields != "" && it.Fields != "{}" {
		_ = json.Unmarshal([]byte(it.Fields), &fieldsMap)
	}
	if models.IsTerminalItem(fieldsMap, cinfo.schema, cinfo.settings) {
		return roleLaneEntry{}, false
	}

	hasLiveLease := leaseHolder.Valid && leaseExpiresAt.Valid && leaseExpiresAt.String > nowStr
	isNow := IsItemNow(&it, cinfo.doneKey, hasLiveLease)
	var lWorker, lUser string
	var lease *models.ItemLease
	if hasLiveLease {
		if leaseWorkerID.Valid {
			lWorker = leaseWorkerID.String
		}
		if leaseUserID.Valid {
			lUser = leaseUserID.String
		}
		lease = &models.ItemLease{
			Holder:     leaseHolder.String,
			Worker:     models.ShortWorkerID(lWorker),
			AcquiredAt: parseTime(leaseAcquiredAt.String),
			ExpiresAt:  parseTime(leaseExpiresAt.String),
		}
	}
	return roleLaneEntry{
		item:          it,
		inQueue:       cinfo.tracksWork && !isNow,
		isNow:         isNow,
		doneKey:       cinfo.doneKey,
		tracksWork:    cinfo.tracksWork,
		leaseWorkerID: lWorker,
		leaseUserID:   lUser,
		lease:         lease,
	}, true
}

// LoadRoleLane returns the canonical full lane for (workspaceID, roleID) in a
// single query pair (collections + role items), sorted with SortRoleLane.
func (s *Store) LoadRoleLane(workspaceID, roleID string) ([]RoleLaneEntry, error) {
	entries, err := s.loadRoleLaneQ(s.db, workspaceID, roleID, now())
	if err != nil {
		return nil, err
	}
	out := make([]RoleLaneEntry, len(entries))
	for i, e := range entries {
		out[i] = toPublicRoleLaneEntry(e)
	}
	return out, nil
}

// LoadWorkspaceRoleLanesQ returns the canonical full lanes for every role in
// workspaceID in a single query pair (collections + workspace role items),
// keyed by role ID and sorted with SortRoleLane, together with the live
// collection metadata already read in the first query.
func (s *Store) LoadWorkspaceRoleLanesQ(q Queryer, workspaceID string) (map[string][]RoleLaneEntry, map[string]RoleLaneCollectionInfo, error) {
	colls, err := s.loadRoleLaneCollectionsQ(q, workspaceID)
	if err != nil {
		return nil, nil, err
	}

	rows, err := q.Query(s.q(`
		SELECT i.agent_role_id, i.id, i.collection_id, i.title, i.fields, i.role_sort_order, i.created_at, i.updated_at, i.item_number,
		       i.lease_holder, i.lease_worker_id, i.lease_user_id, i.lease_acquired_at, i.lease_expires_at
		FROM items i
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL
		WHERE i.workspace_id = ? AND i.agent_role_id IS NOT NULL AND i.agent_role_id <> '' AND i.deleted_at IS NULL
	`), workspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("load workspace role lanes: %w", err)
	}
	defer rows.Close()

	nowStr := now()
	byRole := make(map[string][]RoleLaneEntry)
	for rows.Next() {
		var roleID string
		var it models.Item
		var createdAt, updatedAt string
		var itemNum sql.NullInt64
		var leaseHolder, leaseWorkerID, leaseUserID, leaseAcquiredAt, leaseExpiresAt sql.NullString
		if err := rows.Scan(
			&roleID, &it.ID, &it.CollectionID, &it.Title, &it.Fields, &it.RoleSortOrder, &createdAt, &updatedAt, &itemNum,
			&leaseHolder, &leaseWorkerID, &leaseUserID, &leaseAcquiredAt, &leaseExpiresAt,
		); err != nil {
			return nil, nil, err
		}
		entry, ok := buildRoleLaneEntry(it, createdAt, updatedAt, itemNum, leaseHolder, leaseWorkerID, leaseUserID, leaseAcquiredAt, leaseExpiresAt, colls, nowStr)
		if !ok {
			continue
		}
		byRole[roleID] = append(byRole[roleID], toPublicRoleLaneEntry(entry))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	for roleID := range byRole {
		lane := byRole[roleID]
		sort.SliceStable(lane, func(i, j int) bool {
			return roleLaneLess(lane[i].Item, lane[j].Item)
		})
		byRole[roleID] = lane
	}

	collInfos := make(map[string]RoleLaneCollectionInfo, len(colls))
	for id, ci := range colls {
		collInfos[id] = RoleLaneCollectionInfo{
			ID:          id,
			Schema:      ci.schema,
			Settings:    ci.settings,
			RawSchema:   ci.rawSchema,
			RawSettings: ci.rawSettings,
			Prefix:      ci.prefix,
			DoneKey:     ci.doneKey,
			TracksWork:  ci.tracksWork,
		}
	}
	return byRole, collInfos, nil
}

// FinishedCollectionFilter identifies one work-tracking collection's done field
// and its allowed completed-work to_status spellings (including schema option
// spellings so SQLite's ASCII-only LOWER does not miss non-ASCII case variants).
type FinishedCollectionFilter struct {
	CollectionID string
	FieldKey     string
	Values       []string
}

// maxFinishedVisibilityIDs bounds the visibility IN lists the finished query
// sends. Above it the query applies neither visibility nor its caps; see
// ListWorkspaceRoleFinishedItemsQ.
const maxFinishedVisibilityIDs = 500

// finishedUncapped stands in for the per-role and overall caps when the
// visibility predicate cannot be sent.
const finishedUncapped = math.MaxInt32

// FinishedVisibility scopes ListWorkspaceRoleFinishedItemsQ to items visible
// to the caller before the per-role cap is applied. When Unrestricted is true,
// no visibility predicate is added. Otherwise an item is eligible when its
// collection_id is in CollectionIDs or its id is in ItemIDs; when both slices
// are empty, no query is issued.
type FinishedVisibility struct {
	Unrestricted  bool
	CollectionIDs []string
	ItemIDs       []string
}

// WorkspaceRoleFinishedRow is one candidate status-transition row returned by
// ListWorkspaceRoleFinishedItemsQ for done-key, current-value matching,
// defense-in-depth visibility checking, and per-item deduplication.
type WorkspaceRoleFinishedRow struct {
	ItemID       string
	FieldKey     string
	ToStatus     string
	CreatedAt    time.Time
	CollectionID string
	AgentRoleID  string
	Ref          string
	Title        string
	Fields       string
	Schema       string
	Settings     string
}

// ListWorkspaceRoleFinishedItemsQ returns candidate completion transitions for
// live items in workspaceID assigned to one of roleIDs at or after since,
// matching on each item's current collection against filters and applying vis
// before the per-role cap. It keeps only the newest matching transition per
// item, at most perRole rows per role, and at most limit rows overall, ordered
// newest first (created_at DESC, id DESC).
//
// If either visibility ID list exceeds maxFinishedVisibilityIDs, the query
// sends no visibility predicate and applies neither cap, so hidden rows can
// never use up a role's cap before its visible ones. The caller then filters
// visibility and caps per role in Go. The result is still bounded: one row per
// item completed in the window.
func (s *Store) ListWorkspaceRoleFinishedItemsQ(
	q Queryer,
	workspaceID string,
	roleIDs []string,
	filters []FinishedCollectionFilter,
	vis FinishedVisibility,
	since time.Time,
	perRole int,
	limit int,
) ([]WorkspaceRoleFinishedRow, error) {
	if len(roleIDs) == 0 || len(filters) == 0 || perRole <= 0 || limit <= 0 {
		return nil, nil
	}

	effectivePerRole := perRole
	effectiveLimit := limit
	var visSQL string
	var visArgs []any
	if !vis.Unrestricted {
		if len(vis.CollectionIDs) == 0 && len(vis.ItemIDs) == 0 {
			return nil, nil
		}
		if len(vis.CollectionIDs) > maxFinishedVisibilityIDs || len(vis.ItemIDs) > maxFinishedVisibilityIDs {
			// Oversized IN lists: no SQL visibility, so no SQL caps either
			// (a cap applied before visibility would let hidden rows crowd
			// out visible ones). The caller filters and caps in Go.
			effectivePerRole = finishedUncapped
			effectiveLimit = finishedUncapped
		} else {
			var parts []string
			if len(vis.CollectionIDs) > 0 {
				parts = append(parts, "i.collection_id IN ("+placeholders(len(vis.CollectionIDs))+")")
				for _, cid := range vis.CollectionIDs {
					visArgs = append(visArgs, cid)
				}
			}
			if len(vis.ItemIDs) > 0 {
				parts = append(parts, "i.id IN ("+placeholders(len(vis.ItemIDs))+")")
				for _, iid := range vis.ItemIDs {
					visArgs = append(visArgs, iid)
				}
			}
			visSQL = "\n\t\t\t  AND (" + strings.Join(parts, " OR ") + ")"
		}
	}

	args := make([]any, 0, 3+len(roleIDs)+len(filters)*6+len(visArgs)+2)
	args = append(args, workspaceID, since.UTC().Format(time.RFC3339), workspaceID)
	for _, rid := range roleIDs {
		args = append(args, rid)
	}

	var filterClauses []string
	for _, f := range filters {
		if f.CollectionID == "" || f.FieldKey == "" || len(f.Values) == 0 {
			continue
		}
		trimmed := make([]string, 0, len(f.Values))
		lowered := make([]string, 0, len(f.Values))
		seenTrim := make(map[string]bool, len(f.Values))
		seenLow := make(map[string]bool, len(f.Values))
		for _, v := range f.Values {
			tv := strings.TrimSpace(v)
			if tv == "" {
				continue
			}
			if !seenTrim[tv] {
				seenTrim[tv] = true
				trimmed = append(trimmed, tv)
			}
			lv := strings.ToLower(tv)
			if !seenLow[lv] {
				seenLow[lv] = true
				lowered = append(lowered, lv)
			}
		}
		if len(trimmed) == 0 {
			continue
		}
		clause := "(i.collection_id = ? AND st.field_key = ? AND (TRIM(st.to_status) IN (" +
			placeholders(len(trimmed)) + ") OR LOWER(TRIM(st.to_status)) IN (" +
			placeholders(len(lowered)) + "))"
		args = append(args, f.CollectionID, f.FieldKey)
		for _, v := range trimmed {
			args = append(args, v)
		}
		for _, v := range lowered {
			args = append(args, v)
		}
		if isValidFieldKey(f.FieldKey) {
			curVal := "TRIM(" + s.dialect.JSONExtractText("i.fields", f.FieldKey) + ")"
			clause += " AND (" + curVal + " = TRIM(st.to_status) OR LOWER(" + curVal + ") = LOWER(TRIM(st.to_status)))"
		}
		clause += ")"
		filterClauses = append(filterClauses, clause)
	}
	if len(filterClauses) == 0 {
		return nil, nil
	}
	args = append(args, visArgs...)
	args = append(args, effectivePerRole, effectiveLimit)

	rows, err := q.Query(s.q(`
		WITH item_ranked AS (
			SELECT st.id, st.item_id, st.field_key, st.to_status, st.created_at,
			       i.collection_id, i.agent_role_id, i.title, i.fields, i.item_number,
			       c.prefix, c.schema, c.settings,
			       ROW_NUMBER() OVER (PARTITION BY st.item_id ORDER BY st.created_at DESC, st.id DESC) AS item_rn
			FROM status_transitions st
			JOIN items i ON i.id = st.item_id
			JOIN collections c ON c.id = i.collection_id
			WHERE st.workspace_id = ?
			  AND st.created_at >= ?
			  AND i.workspace_id = ?
			  AND i.deleted_at IS NULL
			  AND c.deleted_at IS NULL
			  AND i.agent_role_id IN (`+placeholders(len(roleIDs))+`)
			  AND (`+strings.Join(filterClauses, " OR ")+`)`+visSQL+`
		),
		role_ranked AS (
			SELECT id, item_id, field_key, to_status, created_at,
			       collection_id, agent_role_id, title, fields, item_number,
			       prefix, schema, settings,
			       ROW_NUMBER() OVER (PARTITION BY agent_role_id ORDER BY created_at DESC, id DESC) AS role_rn
			FROM item_ranked
			WHERE item_rn = 1
		)
		SELECT item_id, field_key, to_status, created_at,
		       collection_id, agent_role_id, title, fields, item_number,
		       prefix, schema, settings
		FROM role_ranked
		WHERE role_rn <= ?
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`), args...)
	if err != nil {
		return nil, fmt.Errorf("list workspace role finished items: %w", err)
	}
	defer rows.Close()

	var out []WorkspaceRoleFinishedRow
	for rows.Next() {
		var r WorkspaceRoleFinishedRow
		var itemNum sql.NullInt64
		var prefix, createdAt string
		if err := rows.Scan(
			&r.ItemID, &r.FieldKey, &r.ToStatus, &createdAt,
			&r.CollectionID, &r.AgentRoleID, &r.Title, &r.Fields, &itemNum,
			&prefix, &r.Schema, &r.Settings,
		); err != nil {
			return nil, err
		}
		if prefix != "" && itemNum.Valid {
			r.Ref = prefix + "-" + strconv.Itoa(int(itemNum.Int64))
		} else {
			r.Ref = r.ItemID
		}
		r.CreatedAt = parseTime(createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
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
		info.rawSchema = rawSchema
		info.rawSettings = rawSettings
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
		SELECT i.id, i.collection_id, i.title, i.fields, i.role_sort_order, i.created_at, i.updated_at, i.item_number,
		       i.lease_holder, i.lease_worker_id, i.lease_user_id, i.lease_acquired_at, i.lease_expires_at
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
		var createdAt, updatedAt string
		var itemNum sql.NullInt64
		var leaseHolder, leaseWorkerID, leaseUserID, leaseAcquiredAt, leaseExpiresAt sql.NullString
		if err := rows.Scan(
			&it.ID, &it.CollectionID, &it.Title, &it.Fields, &it.RoleSortOrder, &createdAt, &updatedAt, &itemNum,
			&leaseHolder, &leaseWorkerID, &leaseUserID, &leaseAcquiredAt, &leaseExpiresAt,
		); err != nil {
			return nil, err
		}
		entry, ok := buildRoleLaneEntry(it, createdAt, updatedAt, itemNum, leaseHolder, leaseWorkerID, leaseUserID, leaseAcquiredAt, leaseExpiresAt, colls, nowStr)
		if !ok {
			continue
		}
		entries = append(entries, entry)
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
// so the exact same items stay above and below the park line. The role's
// other live items (terminal ones and ones in archived collections) follow
// the lane in their old canonical order, so the respace covers every row the
// arrival fast path's MAX reads, and an item that reopens or whose collection
// is restored comes back below the lane: parked under any line, never above.
// The returned lane holds the canonical lane only.
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

	// The rows outside the lane, so no row keeps a value the fast path reads.
	inLane := make(map[string]bool, len(lane))
	for i := range lane {
		inLane[lane[i].item.ID] = true
	}
	rows, err := tx.Query(s.q(`
		SELECT id, role_sort_order, created_at, item_number
		FROM items
		WHERE workspace_id = ? AND agent_role_id = ? AND deleted_at IS NULL
	`), workspaceID, roleID)
	if err != nil {
		return nil, fmt.Errorf("respace load rows outside lane: %w", err)
	}
	var rest []models.Item
	for rows.Next() {
		var it models.Item
		var createdAt string
		var itemNum sql.NullInt64
		if err := rows.Scan(&it.ID, &it.RoleSortOrder, &createdAt, &itemNum); err != nil {
			rows.Close()
			return nil, err
		}
		if inLane[it.ID] {
			continue
		}
		it.CreatedAt = parseTime(createdAt)
		if itemNum.Valid {
			n := int(itemNum.Int64)
			it.ItemNumber = &n
		}
		rest = append(rest, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	SortRoleLane(rest)
	for j := range rest {
		newOrder := roleSortStep * (len(lane) + j + 1)
		if rest[j].RoleSortOrder != newOrder {
			if _, err := stmt.Exec(newOrder, workspaceID, rest[j].ID, workspaceID); err != nil {
				return nil, fmt.Errorf("respace update item %s: %w", rest[j].ID, err)
			}
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
// arriving in roleIDOrSlug's lane: max(role_sort_order over every live item
// of the role, P if mode line else 0) + 1024. If that value would pass 2^30,
// the role is respaced first (which also remaps P) and the value recomputed.
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

	// Fast path: one indexed MAX over the role's live items (terminal ones
	// and ones in archived collections included: they never sit in the queue,
	// so counting them only moves the arrival further down). Every item create
	// and role change runs this under the workspace seq lock, so it must not
	// load the lane's whole history. Only when the value would pass the limit
	// does it respace. respaceRoleLane renumbers exactly the rows this MAX
	// reads, so after one respace the next arrival is fast again; a role still
	// past the limit after a respace needs over 2^20 live rows.
	fastNext := func(mode string, p sql.NullInt64) (int64, error) {
		var laneMax sql.NullInt64
		if err := tx.QueryRow(s.q(`
			SELECT MAX(role_sort_order)
			FROM items
			WHERE workspace_id = ? AND agent_role_id = ? AND deleted_at IS NULL
		`), workspaceID, resolvedRoleID).Scan(&laneMax); err != nil {
			return 0, fmt.Errorf("role arrival lane max: %w", err)
		}
		base := int64(0)
		if laneMax.Valid {
			base = laneMax.Int64
		}
		if mode == models.ParkModeLine && p.Valid && p.Int64 > base {
			base = p.Int64
		}
		return base + roleSortStep, nil
	}

	cand, err := fastNext(parkMode, parkSortOrder)
	if err != nil {
		return 0, err
	}
	if cand <= roleSortRespaceLimit {
		return int(cand), nil
	}

	if _, err := s.respaceRoleLane(tx, workspaceID, resolvedRoleID); err != nil {
		return 0, err
	}
	if err := tx.QueryRow(s.q(`
		SELECT park_mode, park_sort_order
		FROM agent_roles
		WHERE id = ? AND workspace_id = ?
	`), resolvedRoleID, workspaceID).Scan(&parkMode, &parkSortOrder); err != nil {
		return 0, fmt.Errorf("role arrival reload park after respace: %w", err)
	}
	cand, err = fastNext(parkMode, parkSortOrder)
	if err != nil {
		return 0, err
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
		       (SELECT CASE WHEN u.name <> '' THEN u.name ELSE u.username END FROM users u WHERE u.id = r.park_set_by_user_id)
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
