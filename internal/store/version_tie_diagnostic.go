package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/diff"
)

// VersionTieReport counts, for every group of an item's versions that share
// one created_at second, whether the order history reads them in still
// reconstructs their content (BUG-3751). It holds counts only: no ids, no
// content.
//
// Versions are stored as reverse patches, so history rebuilds each one by
// walking back from the item's current content, newest first, in the order
// (created_at DESC, version_seq DESC). Postgres migration 054 assigned
// version_seq to rows that already existed ordering ties by ctid, which is a
// physical location, not chronology, so a tie group can have been numbered
// out of order. Whether an order is right shows only in whether the patches
// apply along it.
type VersionTieReport struct {
	ItemsScanned int `json:"items_scanned"`
	TieGroups    int `json:"tie_groups"`
	// CurrentOrderOnly: the order history uses applies, and no other does.
	CurrentOrderOnly int `json:"current_order_only"`
	// Ambiguous: the current order applies, and so does another. The patches
	// touch text far enough apart that the order cannot be told; harmless
	// when the results agree, which this does not check.
	Ambiguous int `json:"ambiguous"`
	// Misordered: the current order does not apply, and another does. These
	// are the groups a corrective renumbering would fix.
	Misordered int `json:"misordered"`
	// Unresolvable: no order of the group applies. Not an ordering problem.
	Unresolvable int `json:"unresolvable"`
	// SkippedLarge: groups over maxTiePermute rows, not tried.
	SkippedLarge int `json:"skipped_large"`
	// ChainBroken: items whose walk failed on a version outside any tie
	// group, so the groups behind it could not be judged.
	ChainBroken int `json:"chain_broken"`
}

// maxTiePermute bounds the permutations tried per tie group (5! = 120).
const maxTiePermute = 5

type tieVersion struct {
	createdAt string
	content   string
	isDiff    bool
}

// DiagnoseVersionTies reads every item that has same-second versions and
// classifies each tie group (see VersionTieReport). It only reads, inside a
// read-only transaction on Postgres, and takes the database handle directly so
// an operator can point it at a production database without the store's
// migrate-on-open.
func DiagnoseVersionTies(ctx context.Context, db *sql.DB, postgres bool) (*VersionTieReport, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: postgres})
	if err != nil {
		return nil, fmt.Errorf("version tie diagnostic: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; nothing to keep

	rebind := func(q string) string {
		if !postgres {
			return q
		}
		var b strings.Builder
		n := 0
		for _, r := range q {
			if r == '?' {
				n++
				b.WriteString("$" + strconv.Itoa(n))
				continue
			}
			b.WriteRune(r)
		}
		return b.String()
	}

	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT item_id FROM item_versions
		GROUP BY item_id, created_at HAVING COUNT(*) > 1`)
	if err != nil {
		return nil, fmt.Errorf("version tie diagnostic: find tied items: %w", err)
	}
	var itemIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		itemIDs = append(itemIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rep := &VersionTieReport{}
	for _, itemID := range itemIDs {
		var current string
		switch err := tx.QueryRowContext(ctx, rebind(`SELECT content FROM items WHERE id = ?`), itemID).Scan(&current); {
		case err == sql.ErrNoRows:
			continue
		case err != nil:
			return nil, fmt.Errorf("version tie diagnostic: read item: %w", err)
		}
		vrows, err := tx.QueryContext(ctx, rebind(`SELECT created_at, content, is_diff FROM item_versions
			WHERE item_id = ? ORDER BY created_at DESC, version_seq DESC`), itemID)
		if err != nil {
			return nil, fmt.Errorf("version tie diagnostic: read versions: %w", err)
		}
		var versions []tieVersion
		for vrows.Next() {
			var v tieVersion
			if err := vrows.Scan(&v.createdAt, &v.content, &v.isDiff); err != nil {
				vrows.Close()
				return nil, err
			}
			versions = append(versions, v)
		}
		vrows.Close()
		if err := vrows.Err(); err != nil {
			return nil, err
		}
		rep.ItemsScanned++
		classifyTieGroups(current, versions, rep)
	}
	return rep, nil
}

// classifyTieGroups walks one item's versions from its current content and
// counts its tie groups into rep.
func classifyTieGroups(current string, versions []tieVersion, rep *VersionTieReport) {
	content := current
	for i := 0; i < len(versions); {
		j := i + 1
		for j < len(versions) && versions[j].createdAt == versions[i].createdAt {
			j++
		}
		group := versions[i:j]
		var next *tieVersion
		if j < len(versions) {
			next = &versions[j]
		}
		if len(group) == 1 {
			out, ok := applyVersion(content, group[0])
			if !ok {
				rep.ChainBroken++
				return
			}
			content, i = out, j
			continue
		}
		rep.TieGroups++
		if len(group) > maxTiePermute {
			rep.SkippedLarge++
			out, ok := applyOrder(content, group, identity(len(group)), next)
			if !ok {
				rep.ChainBroken++
				return
			}
			content, i = out, j
			continue
		}
		currentOut, currentOK := applyOrder(content, group, identity(len(group)), next)
		otherOK, otherOut := false, ""
		permute(len(group), func(order []int) bool {
			if isIdentity(order) {
				return true
			}
			if out, ok := applyOrder(content, group, order, next); ok {
				otherOK, otherOut = true, out
				return false
			}
			return true
		})
		switch {
		case currentOK && otherOK:
			rep.Ambiguous++
			content = currentOut
		case currentOK:
			rep.CurrentOrderOnly++
			content = currentOut
		case otherOK:
			rep.Misordered++
			content = otherOut
		default:
			rep.Unresolvable++
			return
		}
		i = j
	}
}

// applyOrder applies a tie group in the given order, then checks that the
// next older version still applies to the result: two orders of patches that
// each apply can differ in what they leave for the version behind them.
func applyOrder(content string, group []tieVersion, order []int, next *tieVersion) (string, bool) {
	for _, k := range order {
		out, ok := applyVersion(content, group[k])
		if !ok {
			return "", false
		}
		content = out
	}
	if next != nil {
		if _, ok := applyVersion(content, *next); !ok {
			return "", false
		}
	}
	return content, true
}

func applyVersion(content string, v tieVersion) (string, bool) {
	if !v.isDiff {
		return v.content, true
	}
	out, err := diff.ApplyPatch(content, v.content)
	return out, err == nil
}

func identity(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func isIdentity(order []int) bool {
	for i, k := range order {
		if i != k {
			return false
		}
	}
	return true
}

// permute calls fn with each permutation of 0..n-1 until fn returns false.
func permute(n int, fn func([]int) bool) {
	a := identity(n)
	var rec func(k int) bool
	rec = func(k int) bool {
		if k == n {
			return fn(append([]int(nil), a...))
		}
		for i := k; i < n; i++ {
			a[k], a[i] = a[i], a[k]
			if !rec(k + 1) {
				return false
			}
			a[k], a[i] = a[i], a[k]
		}
		return true
	}
	rec(0)
}

// DiagnoseVersionTiesAt opens the database at dsn WITHOUT running migrations,
// runs DiagnoseVersionTies and closes it, so an operator can measure a
// production database without changing it. postgres selects the driver; a
// SQLite path is opened query-only.
func DiagnoseVersionTiesAt(ctx context.Context, dsn string, postgres bool) (*VersionTieReport, error) {
	if err := registerGuardedDrivers(); err != nil {
		return nil, err
	}
	driver, conn := guardedSQLiteDriver, dsn+"?_pragma=query_only(1)&_pragma=busy_timeout(30000)"
	if postgres {
		driver, conn = guardedPostgresDriver, dsn
	}
	db, err := sql.Open(driver, conn)
	if err != nil {
		return nil, fmt.Errorf("version tie diagnostic: open: %w", err)
	}
	defer db.Close()
	return DiagnoseVersionTies(ctx, db, postgres)
}
