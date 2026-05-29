package store

import (
	"encoding/json"
	"fmt"
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

// ReportData is the full report response. This is the stable contract the
// Reports UI (TASK-1633), CLI/MCP (TASK-1635), and charts (TASK-1632) consume.
type ReportData struct {
	Window                string                  `json:"window"`
	Granularity           string                  `json:"granularity"` // "hour" | "day"
	RangeStart            string                  `json:"range_start"` // RFC3339 UTC
	RangeEnd              string                  `json:"range_end"`   // RFC3339 UTC
	Collections           []string                `json:"collections"` // slugs included
	Buckets               []ReportBucket          `json:"buckets"`     // chronological, zero-filled
	Totals                ReportTotals            `json:"totals"`
	CompletedByCollection []ReportCollectionCount `json:"completed_by_collection"`
	StatusDistribution    []ReportStatusCount     `json:"status_distribution"`
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
	start := now.Add(-lookback)
	startStr := start.Format(time.RFC3339)
	endStr := now.Format(time.RFC3339)

	// Resolve the collections in scope (filtered by slug if requested, and by
	// the caller's visible set when scoping is enabled).
	colls, err := s.resolveReportCollections(workspaceID, opts)
	if err != nil {
		return nil, err
	}

	data := &ReportData{
		Window:                window,
		Granularity:           gran,
		RangeStart:            startStr,
		RangeEnd:              endStr,
		Collections:           make([]string, 0, len(colls)),
		Buckets:               []ReportBucket{},
		CompletedByCollection: []ReportCollectionCount{},
		StatusDistribution:    []ReportStatusCount{},
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
		data.Buckets = s.zeroFilledBuckets(start, now, gran, nil, nil)
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

	data.Buckets = s.zeroFilledBuckets(start, now, gran, createdByBucket, completedByBucket)
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

	dist, err := s.reportStatusDistribution(workspaceID, colls)
	if err != nil {
		return nil, err
	}
	data.StatusDistribution = dist

	return data, nil
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
