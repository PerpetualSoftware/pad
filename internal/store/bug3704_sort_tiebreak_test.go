package store

import (
	"strings"
	"testing"
)

// BUG-3704: an offset page is only stable when the order is total, so every
// item-list sort ends on i.id. SQLite happens to return ties in rowid order,
// which hides a missing tie-break from any end-to-end paging walk; this pins
// the clause itself.
func TestBUG3704_EveryItemSortEndsOnTheID(t *testing.T) {
	d := &sqliteDialect{}
	for sort, want := range map[string]string{
		"":                    " ORDER BY i.pinned DESC, i.updated_at DESC, i.id",
		"bad sort!":           " ORDER BY i.pinned DESC, i.updated_at DESC, i.id",
		"updated_at:desc":     " ORDER BY i.updated_at DESC, i.id",
		"title,created_at":    " ORDER BY i.title ASC, i.created_at ASC, i.id",
		"sort_order,_id:desc": " ORDER BY i.sort_order ASC, i.id DESC",
	} {
		got := buildItemSort(sort, d)
		if got != want {
			t.Errorf("buildItemSort(%q) = %q, want %q", sort, got, want)
		}
		if strings.Count(got, "i.id") != 1 {
			t.Errorf("buildItemSort(%q) = %q: i.id must appear exactly once", sort, got)
		}
	}
}
