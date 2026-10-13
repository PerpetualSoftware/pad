package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	// WithSnapshot: groups holding a full-content row (is_diff false). A
	// snapshot replaces the content whatever came before it, so the patches
	// cannot tell such a group's order; it is neither ordered nor misordered.
	WithSnapshot int `json:"with_snapshot"`
	// ItemsWithTies is how many items have a tie group; ItemsScanned falls
	// short of it only when the run stopped at its deadline (Truncated).
	ItemsWithTies int  `json:"items_with_ties"`
	Truncated     bool `json:"truncated"`
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

// DiagnoseOptions bounds a diagnostic run.
type DiagnoseOptions struct {
	// StatementTimeout caps each statement on Postgres (0: 30s).
	StatementTimeout time.Duration
	// Progress, when set, is called after each item with the count done and
	// the total, so a long run shows it is moving.
	Progress func(done, total int)
	// MaxDuration bounds the whole run, and with it how long the read-only
	// transaction holds its snapshot on a production database (0: no bound).
	// A run that reaches it stops and reports what it scanned, Truncated.
	MaxDuration time.Duration
}

// beginDiagnosticTx opens the diagnostic's one transaction, read-only by
// construction on Postgres: BEGIN READ ONLY through TxOptions and again
// with SET TRANSACTION READ ONLY, so a write inside it is refused by the
// server (SQLSTATE 25006) however the code around it is wrong. SQLite has no
// read-only transaction; DiagnoseVersionTiesAt opens it query_only instead.
func beginDiagnosticTx(ctx context.Context, db *sql.DB, postgres bool, timeout time.Duration) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: postgres})
	if err != nil {
		return nil, fmt.Errorf("version tie diagnostic: begin: %w", err)
	}
	if postgres {
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		for _, stmt := range []string{
			"SET TRANSACTION READ ONLY",
			fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds()),
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return nil, fmt.Errorf("version tie diagnostic: %s: %w", stmt, err)
			}
		}
	}
	return tx, nil
}

// DiagnoseVersionTies reads every item that has same-second versions and
// classifies each tie group (see VersionTieReport). It only reads, in one
// read-only transaction, one item at a time, and takes the database handle
// directly so an operator can point it at a production database without the
// store's migrate-on-open.
func DiagnoseVersionTies(ctx context.Context, db *sql.DB, postgres bool, opts DiagnoseOptions) (*VersionTieReport, error) {
	tx, err := beginDiagnosticTx(ctx, db, postgres, opts.StatementTimeout)
	if err != nil {
		return nil, err
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

	rep := &VersionTieReport{ItemsWithTies: len(itemIDs)}
	var deadline time.Time
	if opts.MaxDuration > 0 {
		deadline = time.Now().Add(opts.MaxDuration)
	}
	for _, itemID := range itemIDs {
		if !deadline.IsZero() && time.Now().After(deadline) {
			rep.Truncated = true
			break
		}
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
		if opts.Progress != nil {
			opts.Progress(rep.ItemsScanned, len(itemIDs))
		}
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
		snapshot := false
		for _, v := range group {
			if !v.isDiff {
				snapshot = true
			}
		}
		if snapshot {
			rep.WithSnapshot++
			out, ok := applyOrder(content, group, identity(len(group)), next)
			if !ok {
				rep.ChainBroken++
				return
			}
			content, i = out, j
			continue
		}
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
// SQLite path is opened query-only, which refuses writes on the connection.
func DiagnoseVersionTiesAt(ctx context.Context, dsn string, postgres bool, opts DiagnoseOptions) (*VersionTieReport, error) {
	db, err := openDiagnosticDB(dsn, postgres)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return DiagnoseVersionTies(ctx, db, postgres, opts)
}

func openDiagnosticDB(dsn string, postgres bool) (*sql.DB, error) {
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
	return db, nil
}
