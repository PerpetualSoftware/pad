package store

import (
	"regexp"
	"strings"
	"testing"
)

// BUG-3667: nextTransitionSeqSubquery is an unfiltered MAX(seq)+1 over
// status_transitions, run inside the write transaction of every status
// change, and nothing indexed seq, so each such write scanned every
// workspace's transitions while holding the workspace lock (and SQLite's one
// writer lock). This is the census of the class: every MAX(...)+1 a write
// path computes, planned on the migrated schema, must read an index.
//
// The plan helper and the backend split are BUG-3668's (explainPlan).
func TestBUG3667_SequenceSubqueriesAreIndexed(t *testing.T) {
	s := testStore(t)
	census := []struct{ site, table, query string }{
		{"nextTransitionSeqSubquery (status changes)", "status_transitions",
			strings.Trim(nextTransitionSeqSubquery, "()")},
		{"nextWorkspaceSeqSubquery (every item write)", "items",
			strings.Trim(nextWorkspaceSeqSubquery, "()")},
		{"item number on create", "items",
			`SELECT COALESCE(MAX(item_number), 0) + 1 FROM items WHERE workspace_id = ?`},
		{"version_seq on a version insert", "item_versions",
			`SELECT COALESCE(MAX(version_seq), 0) + 1 FROM item_versions WHERE item_id = ?`},
	}
	for _, c := range census {
		args := []any{}
		if strings.Contains(c.query, "?") {
			args = append(args, "x")
		}
		plan := explainPlan(t, s, c.query, args...)
		var indexed bool
		if s.dialect.Driver() == DriverPostgres {
			indexed = regexp.MustCompile(`(Index Scan|Index Only Scan)( Backward)? using \S+ on `+c.table+`\b`).MatchString(plan) &&
				!strings.Contains(plan, "Seq Scan on "+c.table)
		} else {
			indexed = regexp.MustCompile(`SEARCH ` + c.table + ` USING (COVERING )?INDEX `).MatchString(plan)
		}
		if !indexed {
			t.Errorf("%s: MAX over %s does not read an index:\n%s", c.site, c.table, plan)
		} else {
			t.Logf("%s: %s", c.site, strings.ReplaceAll(strings.TrimSpace(plan), "\n", " | "))
		}
	}
}
