package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Report aggregation (PLAN-1628 / TASK-1630).
//
// GetReport produces a time-windowed project report: created-vs-completed
// throughput bucketed over a window, net flow, completed-by-collection, and a
// current status-distribution snapshot. "Completed" is a status_transitions
// row INTO a *positive* terminal value (a terminal option that isn't a
// negative outcome like rejected/cancelled — see negativeTerminals), counted
// per the collection's done field. Created counts items.created_at.
//
// Everything routes date math through dialect.DateBucket so the same query
// runs on SQLite and Postgres.

// negativeTerminals are terminal status values that represent a NON-shipping
// close (the work didn't complete positively). They're excluded from the
// "completed" throughput so a rejected idea or cancelled task isn't counted as
// a completion. Matched case-insensitively against a collection's terminal
// options. (A future task can make this per-collection configurable; for now
// it's a sensible global default — PLAN-1628 decision.)
var negativeTerminals = map[string]bool{
	"rejected":  true,
	"cancelled": true,
	"canceled":  true,
	"wontfix":   true,
	"won't fix": true,
	"duplicate": true,
	"declined":  true,
	"abandoned": true,
}

// ReportOptions parameterizes GetReport.
type ReportOptions struct {
	// Window is one of "day", "week", "2wk", "month". Invalid/empty defaults
	// to "week".
	Window string
	// Collections optionally restricts the report to these collection slugs.
	// Empty means all of the workspace's collections.
	Collections []string
	// Now is the reference end of the window; zero means time.Now().UTC().
	// Injectable so tests are deterministic.
	Now time.Time
	// Offset steps the window back by whole window-lengths (0 = current period,
	// 1 = the period immediately before, …). Negative is clamped to 0.
	Offset int
	// IncludeItems, when true, populates CompletedItems (the "what shipped"
	// list) — opt-in because the interactive dashboard only needs counts; the
	// print/export report requests it.
	IncludeItems bool
	// ScopeToVisible, when true, restricts the report to VisibleCollectionIDs
	// (the caller's visible collection set). Aggregate counts for collections
	// the caller can't see are never computed — preventing a member/guest from
	// inferring hidden collections' throughput or status distribution. The
	// HTTP handler sets this from visibleCollectionIDs(); internal/CLI callers
	// that already run as an authorized principal leave it false for the full
	// workspace report.
	ScopeToVisible       bool
	VisibleCollectionIDs []string
}

// completedItemsCap bounds the "what shipped" list so a pathological window
// can't produce an unbounded payload; the rest is reported via the overflow
// count.
const completedItemsCap = 500

// ReportBucket is one time-series point: items created and completed within
// the bucket. Bucket is a sortable UTC label ("YYYY-MM-DD", or
// "YYYY-MM-DDTHH" for the hourly window).
type ReportBucket struct {
	Bucket    string `json:"bucket"`
	Created   int    `json:"created"`
	Completed int    `json:"completed"`
}

// ReportCollectionCount is a per-collection tally (collection slug + count).
type ReportCollectionCount struct {
	Collection string `json:"collection"`
	Count      int    `json:"count"`
}

// ReportStatusCount is a current status-distribution entry.
type ReportStatusCount struct {
	Collection string `json:"collection"`
	Status     string `json:"status"`
	Count      int    `json:"count"`
}

// ReportTotals are the window roll-ups.
type ReportTotals struct {
	Created   int `json:"created"`
	Completed int `json:"completed"`
	NetFlow   int `json:"net_flow"` // Created - Completed
}

// ReportDuration is a per-collection median duration (hours) over a sample.
type ReportDuration struct {
	Collection  string  `json:"collection"`
	Count       int     `json:"count"`
	MedianHours float64 `json:"median_hours"`
}

// ReportCycleTime summarizes time from item creation to a positive-terminal
// transition, for completions within the window. Medians/percentiles are
// computed in Go from raw durations (dual-dialect: no SQL percentile).
type ReportCycleTime struct {
	SampleSize   int              `json:"sample_size"`
	MedianHours  float64          `json:"median_hours"`
	P90Hours     float64          `json:"p90_hours"`
	ByCollection []ReportDuration `json:"by_collection"`
}

// ReportAgingBucket is a count of currently-open items in an age band.
type ReportAgingBucket struct {
	Label string `json:"label"` // "<1d" | "1-7d" | "7-30d" | ">30d"
	Count int    `json:"count"`
}

// ReportWIP is a point-in-time snapshot of work-in-progress: items whose done
// field is NOT a terminal value (open), how long they've sat, and an age
// distribution. Not windowed — reflects the current open set.
type ReportWIP struct {
	OpenCount      int                 `json:"open_count"`
	MedianAgeHours float64             `json:"median_age_hours"`
	AgingBuckets   []ReportAgingBucket `json:"aging_buckets"`
	ByCollection   []ReportDuration    `json:"by_collection"` // MedianHours = median open-item age
}

// ReportCompletedItem is one item that reached a positive terminal within the
// window — the "what shipped" list for the exec report (TASK-1641).
type ReportCompletedItem struct {
	Ref         string `json:"ref"` // e.g. TASK-5
	Title       string `json:"title"`
	Collection  string `json:"collection"`   // slug
	CompletedAt string `json:"completed_at"` // RFC3339, latest completion in-window
}

// ReportData is the full report response. This is the stable contract the
// Reports UI (TASK-1633), CLI/MCP (TASK-1635), and charts (TASK-1632) consume.
type ReportData struct {
	Window                string                  `json:"window"`
	Offset                int                     `json:"offset"`      // periods back from now (0 = current)
	Granularity           string                  `json:"granularity"` // "hour" | "day"
	RangeStart            string                  `json:"range_start"` // RFC3339 UTC
	RangeEnd              string                  `json:"range_end"`   // RFC3339 UTC
	Collections           []string                `json:"collections"` // slugs included
	Buckets               []ReportBucket          `json:"buckets"`     // chronological, zero-filled
	Totals                ReportTotals            `json:"totals"`
	CompletedByCollection []ReportCollectionCount `json:"completed_by_collection"`
	StatusDistribution    []ReportStatusCount     `json:"status_distribution"`
	CycleTime             ReportCycleTime         `json:"cycle_time"`
	WIP                   ReportWIP               `json:"wip"`
	// CompletedItems is the "what shipped" list, populated only when
	// IncludeItems was requested (nil/omitted otherwise). Deduped by item,
	// newest completion first, capped at completedItemsCap with the remainder
	// reported in CompletedItemsOverflowCount.
	CompletedItems         []ReportCompletedItem `json:"completed_items,omitempty"`
	CompletedItemsOverflow int                   `json:"completed_items_overflow_count,omitempty"`
}

// windowSpec maps a window to its lookback duration and bucket granularity.
func windowSpec(window string) (lookback time.Duration, granularity string) {
	switch window {
	case "day":
		return 24 * time.Hour, "hour"
	case "2wk":
		return 14 * 24 * time.Hour, "day"
	case "month":
		return 30 * 24 * time.Hour, "day"
	default: // "week"
		return 7 * 24 * time.Hour, "day"
	}
}

// reportCollection is a resolved collection with its done-field + the positive
// terminal values that count as a completion.
type reportCollection struct {
	id                string
	slug              string
	doneKey           string
	positiveTerminals []string
	allTerminals      []string // positive + negative; used to identify still-open (WIP) items
}

// GetReport computes the windowed project report for a workspace.
func (s *Store) GetReport(workspaceID string, opts ReportOptions) (*ReportData, error) {
	window := opts.Window
	switch window {
	case "day", "week", "2wk", "month":
	default:
		window = "week"
	}
	lookback, gran := windowSpec(window)

	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()

	// offset steps the window back by whole window-lengths (0 = current,
	// 1 = the window immediately before, …). Negative is clamped to 0 (no
	// future). end is the window's right edge; start its left edge.
	offset := opts.Offset
	if offset < 0 {
		offset = 0
	}
	end := now.Add(-time.Duration(offset) * lookback)
	start := end.Add(-lookback)
	startStr := start.Format(time.RFC3339)
	endStr := end.Format(time.RFC3339)

	// Resolve the collections in scope (filtered by slug if requested, and by
	// the caller's visible set when scoping is enabled).
	colls, err := s.resolveReportCollections(workspaceID, opts)
	if err != nil {
		return nil, err
	}

	data := &ReportData{
		Window:                window,
		Offset:                offset,
		Granularity:           gran,
		RangeStart:            startStr,
		RangeEnd:              endStr,
		Collections:           make([]string, 0, len(colls)),
		Buckets:               []ReportBucket{},
		CompletedByCollection: []ReportCollectionCount{},
		StatusDistribution:    []ReportStatusCount{},
		// Initialize nested slices so they marshal as [] (not null) on every
		// path — including the empty-scope early return below. Matches the TS
		// contract (ReportCycleTime.by_collection, ReportWIP.aging_buckets /
		// by_collection are always arrays).
		CycleTime: ReportCycleTime{ByCollection: []ReportDuration{}},
		WIP:       ReportWIP{AgingBuckets: []ReportAgingBucket{}, ByCollection: []ReportDuration{}},
	}
	collIDs := make([]string, 0, len(colls))
	slugByID := make(map[string]string, len(colls))
	for _, c := range colls {
		data.Collections = append(data.Collections, c.slug)
		collIDs = append(collIDs, c.id)
		slugByID[c.id] = c.slug
	}

	// No collections in scope → empty (but well-formed) report.
	if len(collIDs) == 0 {
		data.Buckets = s.zeroFilledBuckets(start, end, gran, nil, nil)
		return data, nil
	}

	createdByBucket, err := s.reportCreatedBuckets(workspaceID, collIDs, startStr, endStr, gran)
	if err != nil {
		return nil, err
	}
	completedByBucket, completedByColl, err := s.reportCompletedBuckets(workspaceID, colls, startStr, endStr, gran)
	if err != nil {
		return nil, err
	}

	data.Buckets = s.zeroFilledBuckets(start, end, gran, createdByBucket, completedByBucket)
	for _, b := range data.Buckets {
		data.Totals.Created += b.Created
		data.Totals.Completed += b.Completed
	}
	data.Totals.NetFlow = data.Totals.Created - data.Totals.Completed

	// completed_by_collection (whole window), ordered by count desc then slug.
	for id, n := range completedByColl {
		data.CompletedByCollection = append(data.CompletedByCollection, ReportCollectionCount{
			Collection: slugByID[id], Count: n,
		})
	}
	sort.Slice(data.CompletedByCollection, func(i, j int) bool {
		a, b := data.CompletedByCollection[i], data.CompletedByCollection[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Collection < b.Collection
	})

	cycle, err := s.reportCycleTime(workspaceID, colls, startStr, endStr, slugByID)
	if err != nil {
		return nil, err
	}
	data.CycleTime = cycle

	// Snapshot metrics (status distribution + WIP). At offset 0 they reflect
	// "now" from the live items table (ground truth). For a PAST period
	// (offset > 0) we reconstruct them as-of the window end from the
	// status-transition history, so they reflect the selected period rather
	// than the current moment (PLAN-1628 / TASK-1640).
	if offset > 0 {
		dist, wip, rerr := s.reportSnapshotAsOf(workspaceID, colls, end)
		if rerr != nil {
			return nil, rerr
		}
		data.StatusDistribution = dist
		data.WIP = wip
	} else {
		dist, derr := s.reportStatusDistribution(workspaceID, colls)
		if derr != nil {
			return nil, derr
		}
		data.StatusDistribution = dist
		wip, werr := s.reportWIP(workspaceID, colls, now)
		if werr != nil {
			return nil, werr
		}
		data.WIP = wip
	}

	if opts.IncludeItems {
		items, overflow, ierr := s.reportCompletedItems(workspaceID, colls, startStr, endStr)
		if ierr != nil {
			return nil, ierr
		}
		data.CompletedItems = items
		data.CompletedItemsOverflow = overflow
	}

	return data, nil
}

// reportCompletedItems returns the items that reached a positive terminal in
// the window (the "what shipped" list, TASK-1641): deduped by item (newest
// completion first), capped at completedItemsCap with the remainder in the
// returned overflow count. Same positive-terminal source as totals.completed,
// so the list reconciles with the count (modulo the dedupe + cap). Joins live
// items (deleted_at IS NULL), consistent with the completed counts.
func (s *Store) reportCompletedItems(workspaceID string, colls []reportCollection, startStr, endStr string) ([]ReportCompletedItem, int, error) {
	posExpr, posArgs := s.positiveTerminalExpr(colls)
	if posExpr == "" {
		return []ReportCompletedItem{}, 0, nil
	}

	// Restrict item-level rows to items whose CURRENT collection is in scope.
	// posExpr scopes by st.collection_id (the collection at completion time),
	// but this query returns the item's live title/ref/collection — so an item
	// completed while in a visible collection and since moved to a hidden one
	// must NOT leak its current (hidden) details to a restricted caller.
	curScope := make([]string, 0, len(colls))
	curArgs := make([]any, 0, len(colls))
	for _, c := range colls {
		curScope = append(curScope, "?")
		curArgs = append(curArgs, c.id)
	}
	curExpr := "i.collection_id IN (" + strings.Join(curScope, ",") + ")"

	base := []any{workspaceID, startStr, endStr}

	// Distinct completed-item count (for the overflow figure).
	var total int
	cArgs := append(append([]any{}, base...), posArgs...)
	cArgs = append(cArgs, curArgs...)
	if err := s.db.QueryRow(s.q(fmt.Sprintf(`
		SELECT COUNT(DISTINCT st.item_id)
		FROM status_transitions st
		JOIN items i ON i.id = st.item_id AND i.deleted_at IS NULL
		WHERE st.workspace_id = ? AND st.created_at >= ? AND st.created_at <= ?
		  AND %s AND %s
	`, posExpr, curExpr)), cArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count completed items: %w", err)
	}

	// The list: one row per item (latest in-window completion), newest first.
	lArgs := append(append([]any{}, base...), posArgs...)
	lArgs = append(lArgs, curArgs...)
	lArgs = append(lArgs, completedItemsCap)
	rows, err := s.db.Query(s.q(fmt.Sprintf(`
		SELECT c.prefix, i.item_number, i.title, c.slug, MAX(st.created_at) AS completed_at
		FROM status_transitions st
		JOIN items i ON i.id = st.item_id AND i.deleted_at IS NULL
		JOIN collections c ON c.id = i.collection_id
		WHERE st.workspace_id = ? AND st.created_at >= ? AND st.created_at <= ?
		  AND %s AND %s
		GROUP BY i.id, c.prefix, i.item_number, i.title, c.slug
		ORDER BY completed_at DESC
		LIMIT ?
	`, posExpr, curExpr)), lArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list completed items: %w", err)
	}
	defer rows.Close()

	out := []ReportCompletedItem{}
	for rows.Next() {
		var prefix, title, slug, completedAt string
		var itemNumber int
		if err := rows.Scan(&prefix, &itemNumber, &title, &slug, &completedAt); err != nil {
			return nil, 0, fmt.Errorf("scan completed item: %w", err)
		}
		out = append(out, ReportCompletedItem{
			Ref:         fmt.Sprintf("%s-%d", prefix, itemNumber),
			Title:       title,
			Collection:  slug,
			CompletedAt: completedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	overflow := total - len(out)
	if overflow < 0 {
		overflow = 0
	}
	return out, overflow, nil
}

// reportSnapshotAsOf reconstructs the status-distribution and WIP snapshot as
// they stood at time t, from the status-transition history (PLAN-1628 /
// TASK-1640). An item's status as-of-t is the to_status of its latest
// transition on the collection's done field with created_at <= t. Per
// collection (the done field varies), it scans items currently in that
// collection that existed at t (created_at <= t) and weren't deleted by then
// (deleted_at IS NULL OR > t), resolving each one's as-of-t value via a
// "no later transition <= t" NOT EXISTS — dual-dialect, no window functions.
//
// Accuracy (historical periods are best-effort, matching the rest of the
// feature's historical posture):
//   - exact for post-launch history (the live write path records every
//     status change, including clears); approximate for pre-launch history,
//     since the backfill parsed the debounce-coalesced activity log.
//   - collection attribution uses the item's CURRENT collection, not the one
//     it belonged to at t. An item moved between collections is attributed to
//     its present collection for past periods too. Reconstructing historical
//     collection membership would require replaying move history (and
//     status-preserving moves record no transition), so this is a deliberate
//     limitation — the same current-collection attribution the backfill and
//     completed-by-collection use. Items rarely change collection.
//
// Same-second multi-hops (an item changing status 2+ times within one second)
// are resolved deterministically via the monotonic `seq` tiebreak (TASK-1643):
// "latest transition <= t" orders by created_at, then seq (insertion order),
// then id — so the chronologically-last same-second hop wins.
func (s *Store) reportSnapshotAsOf(workspaceID string, colls []reportCollection, t time.Time) ([]ReportStatusCount, ReportWIP, error) {
	tStr := t.Format(time.RFC3339)
	dist := []ReportStatusCount{}
	wip := ReportWIP{AgingBuckets: []ReportAgingBucket{}, ByCollection: []ReportDuration{}}

	var openAges []float64
	perColl := map[string][]float64{}
	slugByID := map[string]string{}

	for _, c := range colls {
		slugByID[c.id] = c.slug
		terminals := map[string]bool{}
		for _, v := range c.allTerminals {
			terminals[strings.ToLower(v)] = true
		}
		// Start from items (not transitions) so an item that existed at t but
		// had no done-field transition yet is still counted — matching the
		// live path, which treats a missing/empty done value as open. For each
		// in-scope item that existed at t and wasn't deleted by t, the
		// correlated subquery resolves its as-of-t value: the to_status of the
		// latest transition on the done field at or before t (NULL if none →
		// treated as empty/open). LIMIT 1 ORDER BY created_at,id is valid on
		// both dialects (no window functions).
		query := `
			SELECT i.created_at,
			  (SELECT st.to_status FROM status_transitions st
			   WHERE st.item_id = i.id AND st.field_key = ? AND st.created_at <= ?
			   ORDER BY st.created_at DESC, st.seq DESC, st.id DESC LIMIT 1) AS asof_status
			FROM items i
			WHERE i.workspace_id = ? AND i.collection_id = ?
			  AND i.created_at <= ?
			  AND (i.deleted_at IS NULL OR i.deleted_at > ?)
		`
		rows, err := s.db.Query(s.q(query), c.doneKey, tStr, workspaceID, c.id, tStr, tStr)
		if err != nil {
			return nil, ReportWIP{}, fmt.Errorf("reconstruct snapshot: %w", err)
		}
		statusCounts := map[string]int{}
		func() {
			defer rows.Close()
			for rows.Next() {
				var createdAt string
				var asof sql.NullString
				if scanErr := rows.Scan(&createdAt, &asof); scanErr != nil {
					err = fmt.Errorf("scan snapshot row: %w", scanErr)
					return
				}
				status := strings.ToLower(asof.String) // "" when NULL
				// Status distribution skips items with no done value (matches
				// the live path).
				if status != "" {
					statusCounts[status]++
				}
				// WIP: open = not a terminal value (a missing/empty value is
				// open too); age measured against t.
				if !terminals[status] {
					if h, ok := durationHours(createdAt, tStr); ok {
						openAges = append(openAges, h)
						perColl[c.id] = append(perColl[c.id], h)
					}
				}
			}
			err = rows.Err()
		}()
		if err != nil {
			return nil, ReportWIP{}, err
		}
		for status, n := range statusCounts {
			dist = append(dist, ReportStatusCount{Collection: c.slug, Status: status, Count: n})
		}
	}

	sort.Slice(dist, func(i, j int) bool {
		if dist[i].Collection != dist[j].Collection {
			return dist[i].Collection < dist[j].Collection
		}
		return dist[i].Status < dist[j].Status
	})

	wip.OpenCount = len(openAges)
	wip.MedianAgeHours = percentile(openAges, 0.5)
	wip.ByCollection = durationsByCollection(perColl, slugByID)
	wip.AgingBuckets = agingBuckets(openAges)
	return dist, wip, nil
}

// reportCycleTime computes created→positive-terminal durations for completions
// within the window, returning overall median/p90 + per-collection medians.
// Each positive-terminal transition in the window is one sample (an item
// completed twice contributes twice — consistent with the completed counts).
func (s *Store) reportCycleTime(workspaceID string, colls []reportCollection, startStr, endStr string, slugByID map[string]string) (ReportCycleTime, error) {
	posExpr, posArgs := s.positiveTerminalExpr(colls)
	if posExpr == "" {
		return ReportCycleTime{ByCollection: []ReportDuration{}}, nil
	}
	args := append([]any{workspaceID, startStr, endStr}, posArgs...)
	query := fmt.Sprintf(`
		SELECT st.collection_id, i.created_at, st.created_at
		FROM status_transitions st
		JOIN items i ON i.id = st.item_id AND i.deleted_at IS NULL
		WHERE st.workspace_id = ? AND st.created_at >= ? AND st.created_at <= ?
		  AND %s
	`, posExpr)
	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return ReportCycleTime{}, fmt.Errorf("report cycle-time: %w", err)
	}
	defer rows.Close()

	var overall []float64
	perColl := map[string][]float64{}
	for rows.Next() {
		var collID, createdAt, doneAt string
		if err := rows.Scan(&collID, &createdAt, &doneAt); err != nil {
			return ReportCycleTime{}, fmt.Errorf("scan cycle-time: %w", err)
		}
		h, ok := durationHours(createdAt, doneAt)
		if !ok {
			continue
		}
		overall = append(overall, h)
		perColl[collID] = append(perColl[collID], h)
	}
	if err := rows.Err(); err != nil {
		return ReportCycleTime{}, err
	}

	ct := ReportCycleTime{
		SampleSize:   len(overall),
		MedianHours:  percentile(overall, 0.5),
		P90Hours:     percentile(overall, 0.9),
		ByCollection: durationsByCollection(perColl, slugByID),
	}
	return ct, nil
}

// reportWIP snapshots currently-open items (done field NOT a terminal value),
// their age (now - created_at), and an age-band distribution. Point-in-time.
func (s *Store) reportWIP(workspaceID string, colls []reportCollection, now time.Time) (ReportWIP, error) {
	wip := ReportWIP{
		AgingBuckets: []ReportAgingBucket{},
		ByCollection: []ReportDuration{},
	}
	var overall []float64
	perColl := map[string][]float64{}
	slugByID := map[string]string{}

	for _, c := range colls {
		slugByID[c.id] = c.slug
		fieldExpr := s.dialect.JSONExtractText("fields", c.doneKey)
		// Open = done-field value NOT in this collection's terminal set. An
		// item with no done-field value (NULL/"") is also open.
		var notTerminal string
		var args []any
		args = append(args, workspaceID, c.id)
		if len(c.allTerminals) > 0 {
			ph := make([]string, len(c.allTerminals))
			for i, v := range c.allTerminals {
				ph[i] = "?"
				args = append(args, strings.ToLower(v))
			}
			notTerminal = fmt.Sprintf("LOWER(COALESCE(%s, '')) NOT IN (%s)", fieldExpr, strings.Join(ph, ","))
		} else {
			notTerminal = "1=1"
		}
		query := fmt.Sprintf(`
			SELECT created_at FROM items
			WHERE workspace_id = ? AND collection_id = ? AND deleted_at IS NULL
			  AND %s
		`, notTerminal)
		rows, err := s.db.Query(s.q(query), args...)
		if err != nil {
			return ReportWIP{}, fmt.Errorf("report wip: %w", err)
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var createdAt string
				if scanErr := rows.Scan(&createdAt); scanErr != nil {
					err = fmt.Errorf("scan wip: %w", scanErr)
					return
				}
				h, ok := ageHours(createdAt, now)
				if !ok {
					continue
				}
				overall = append(overall, h)
				perColl[c.id] = append(perColl[c.id], h)
			}
			err = rows.Err()
		}()
		if err != nil {
			return ReportWIP{}, err
		}
	}

	wip.OpenCount = len(overall)
	wip.MedianAgeHours = percentile(overall, 0.5)
	wip.ByCollection = durationsByCollection(perColl, slugByID)
	wip.AgingBuckets = agingBuckets(overall)
	return wip, nil
}

// resolveReportCollections lists the workspace's collections (optionally
// filtered to the given slugs) and resolves each one's done field + positive
// terminal values.
func (s *Store) resolveReportCollections(workspaceID string, opts ReportOptions) ([]reportCollection, error) {
	all, err := s.ListCollections(workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list collections for report: %w", err)
	}
	want := map[string]bool{}
	for _, sl := range opts.Collections {
		want[strings.ToLower(strings.TrimSpace(sl))] = true
	}
	// Visibility scope: when enabled, only collections in the caller's visible
	// set are eligible (an empty set yields an empty report — correct for a
	// guest with no full-collection access).
	var visible map[string]bool
	if opts.ScopeToVisible {
		visible = make(map[string]bool, len(opts.VisibleCollectionIDs))
		for _, id := range opts.VisibleCollectionIDs {
			visible[id] = true
		}
	}

	var out []reportCollection
	for _, c := range all {
		if len(want) > 0 && !want[strings.ToLower(c.Slug)] {
			continue
		}
		if visible != nil && !visible[c.ID] {
			continue
		}
		var schema models.CollectionSchema
		var settings models.CollectionSettings
		// Parse failures fall back to status + default terminals, matching
		// scanCollectionDoneFilters' tolerance.
		if c.Schema != "" {
			_ = json.Unmarshal([]byte(c.Schema), &schema)
		}
		if c.Settings != "" {
			_ = json.Unmarshal([]byte(c.Settings), &settings)
		}
		doneKey, terminals := models.TerminalValuesForDoneField(schema, settings)

		var positives []string
		for _, v := range terminals {
			if negativeTerminals[strings.ToLower(strings.TrimSpace(v))] {
				continue
			}
			positives = append(positives, v)
		}
		out = append(out, reportCollection{
			id:                c.ID,
			slug:              c.Slug,
			doneKey:           doneKey,
			positiveTerminals: positives,
			allTerminals:      terminals,
		})
	}
	return out, nil
}

// reportCreatedBuckets returns created-item counts keyed by bucket label.
func (s *Store) reportCreatedBuckets(workspaceID string, collIDs []string, startStr, endStr, gran string) (map[string]int, error) {
	bucketExpr := s.dialect.DateBucket("created_at", gran)
	placeholders := make([]string, len(collIDs))
	args := []any{workspaceID, startStr, endStr}
	for i, id := range collIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := fmt.Sprintf(`
		SELECT %s AS bucket, COUNT(*)
		FROM items
		WHERE workspace_id = ? AND deleted_at IS NULL
		  AND created_at >= ? AND created_at <= ?
		  AND collection_id IN (%s)
		GROUP BY bucket
	`, bucketExpr, strings.Join(placeholders, ","))

	return s.scanBucketCounts(query, args)
}

// reportCompletedBuckets returns completion counts keyed by bucket label AND
// the per-collection completion totals over the whole window. A completion is
// a status_transitions row into a positive terminal for that collection's done
// field.
func (s *Store) reportCompletedBuckets(workspaceID string, colls []reportCollection, startStr, endStr, gran string) (byBucket map[string]int, byColl map[string]int, err error) {
	posExpr, posArgs := s.positiveTerminalExpr(colls)
	byBucket = map[string]int{}
	byColl = map[string]int{}
	if posExpr == "" {
		// No collection has positive terminals → nothing completes.
		return byBucket, byColl, nil
	}

	base := []any{workspaceID, startStr, endStr}
	bucketExpr := s.dialect.DateBucket("st.created_at", gran)

	// Join live items so a completed-then-soft-deleted item is excluded from
	// the completion counts too — keeping them consistent with the created and
	// status-distribution queries (which already filter deleted_at IS NULL).
	// status_transitions rows survive a soft delete (only a HARD delete
	// cascades them away).
	const liveJoin = "JOIN items i ON i.id = st.item_id AND i.deleted_at IS NULL"

	// Buckets.
	bArgs := append(append([]any{}, base...), posArgs...)
	bQuery := fmt.Sprintf(`
		SELECT %s AS bucket, COUNT(*)
		FROM status_transitions st
		%s
		WHERE st.workspace_id = ? AND st.created_at >= ? AND st.created_at <= ?
		  AND %s
		GROUP BY bucket
	`, bucketExpr, liveJoin, posExpr)
	byBucket, err = s.scanBucketCounts(bQuery, bArgs)
	if err != nil {
		return nil, nil, err
	}

	// Per-collection totals.
	cArgs := append(append([]any{}, base...), posArgs...)
	cQuery := fmt.Sprintf(`
		SELECT st.collection_id, COUNT(*)
		FROM status_transitions st
		%s
		WHERE st.workspace_id = ? AND st.created_at >= ? AND st.created_at <= ?
		  AND %s
		GROUP BY st.collection_id
	`, liveJoin, posExpr)
	rows, err := s.db.Query(s.q(cQuery), cArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("report completed-by-collection: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, nil, fmt.Errorf("scan completed-by-collection: %w", err)
		}
		byColl[id] = n
	}
	return byBucket, byColl, rows.Err()
}

// positiveTerminalExpr builds the "this transition is a positive completion"
// boolean: OR over collections of
// (st.collection_id = ? AND st.field_key = ? AND LOWER(st.to_status) IN (...)).
// Returns "" (no args) when no collection has any positive terminal value.
func (s *Store) positiveTerminalExpr(colls []reportCollection) (string, []any) {
	var clauses []string
	var args []any
	for _, c := range colls {
		if len(c.positiveTerminals) == 0 {
			continue
		}
		ph := make([]string, len(c.positiveTerminals))
		args = append(args, c.id, c.doneKey)
		for i, v := range c.positiveTerminals {
			ph[i] = "?"
			args = append(args, strings.ToLower(v))
		}
		clauses = append(clauses, fmt.Sprintf(
			"(st.collection_id = ? AND st.field_key = ? AND LOWER(st.to_status) IN (%s))",
			strings.Join(ph, ","),
		))
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

// reportStatusDistribution returns the current per-collection breakdown of
// items by their done-field value. One small grouped query per collection
// (the done field differs per collection, so a single GROUP BY can't span
// them).
func (s *Store) reportStatusDistribution(workspaceID string, colls []reportCollection) ([]ReportStatusCount, error) {
	out := []ReportStatusCount{}
	for _, c := range colls {
		fieldExpr := s.dialect.JSONExtractText("fields", c.doneKey)
		query := fmt.Sprintf(`
			SELECT LOWER(COALESCE(%s, '')) AS st, COUNT(*)
			FROM items
			WHERE workspace_id = ? AND collection_id = ? AND deleted_at IS NULL
			GROUP BY st
		`, fieldExpr)
		rows, err := s.db.Query(s.q(query), workspaceID, c.id)
		if err != nil {
			return nil, fmt.Errorf("report status distribution: %w", err)
		}
		func() {
			defer rows.Close()
			for rows.Next() {
				var status string
				var n int
				if scanErr := rows.Scan(&status, &n); scanErr != nil {
					err = fmt.Errorf("scan status distribution: %w", scanErr)
					return
				}
				if status == "" {
					continue // items with no done-field value set
				}
				out = append(out, ReportStatusCount{Collection: c.slug, Status: status, Count: n})
			}
			err = rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanBucketCounts runs a "SELECT <bucket>, COUNT(*) ... GROUP BY <bucket>"
// query and returns a label→count map.
func (s *Store) scanBucketCounts(query string, args []any) (map[string]int, error) {
	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, fmt.Errorf("report bucket query: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var bucket string
		var n int
		if err := rows.Scan(&bucket, &n); err != nil {
			return nil, fmt.Errorf("scan report bucket: %w", err)
		}
		out[bucket] = n
	}
	return out, rows.Err()
}

// zeroFilledBuckets produces the full chronological bucket series between start
// and end (inclusive) at the given granularity, merging in created/completed
// counts (nil maps are treated as empty). Continuous buckets make the series
// chart-ready without client-side gap-filling.
func (s *Store) zeroFilledBuckets(start, end time.Time, gran string, created, completed map[string]int) []ReportBucket {
	var step time.Duration
	var layout string
	if gran == "hour" {
		step, layout = time.Hour, "2006-01-02T15"
		start = start.Truncate(time.Hour)
	} else {
		step, layout = 24*time.Hour, "2006-01-02"
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	}
	out := []ReportBucket{}
	for t := start; !t.After(end); t = t.Add(step) {
		label := t.UTC().Format(layout)
		out = append(out, ReportBucket{
			Bucket:    label,
			Created:   created[label],
			Completed: completed[label],
		})
	}
	return out
}

// --- cycle-time / WIP helpers (PLAN-1628 / TASK-1631) ---

// durationHours returns hours between two RFC3339 timestamps (end-start),
// clamped at 0. ok is false when either fails to parse.
func durationHours(startStr, endStr string) (float64, bool) {
	st, e1 := time.Parse(time.RFC3339, startStr)
	en, e2 := time.Parse(time.RFC3339, endStr)
	if e1 != nil || e2 != nil {
		return 0, false
	}
	h := en.Sub(st).Hours()
	if h < 0 {
		h = 0
	}
	return h, true
}

// ageHours returns hours from an RFC3339 created timestamp to now, clamped at 0.
func ageHours(createdStr string, now time.Time) (float64, bool) {
	c, err := time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return 0, false
	}
	h := now.Sub(c).Hours()
	if h < 0 {
		h = 0
	}
	return h, true
}

// percentile returns the linearly-interpolated p-quantile (p in [0,1]) of vals,
// rounded to 1 decimal. 0 for an empty sample. Computed in Go so the same code
// runs identically on SQLite and Postgres (neither has a portable MEDIAN).
func percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	if len(s) == 1 {
		return round1(s[0])
	}
	rank := p * float64(len(s)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return round1(s[lo])
	}
	frac := rank - float64(lo)
	return round1(s[lo] + (s[hi]-s[lo])*frac)
}

func round1(x float64) float64 { return math.Round(x*10) / 10 }

// durationsByCollection turns per-collection samples into median+count entries,
// ordered by count desc then slug.
func durationsByCollection(perColl map[string][]float64, slugByID map[string]string) []ReportDuration {
	out := []ReportDuration{}
	for id, vals := range perColl {
		out = append(out, ReportDuration{
			Collection:  slugByID[id],
			Count:       len(vals),
			MedianHours: percentile(vals, 0.5),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Collection < out[j].Collection
	})
	return out
}

// agingBuckets distributes open-item ages (hours) into fixed bands. Always
// returns all four bands in order (zero counts included) for a stable shape.
func agingBuckets(ages []float64) []ReportAgingBucket {
	buckets := []ReportAgingBucket{{Label: "<1d"}, {Label: "1-7d"}, {Label: "7-30d"}, {Label: ">30d"}}
	for _, h := range ages {
		switch {
		case h < 24:
			buckets[0].Count++
		case h < 24*7:
			buckets[1].Count++
		case h < 24*30:
			buckets[2].Count++
		default:
			buckets[3].Count++
		}
	}
	return buckets
}
