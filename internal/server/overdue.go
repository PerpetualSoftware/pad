package server

import (
	"strings"
	"time"
)

// The one place that decides whether an item is overdue (IDEA-2641).
//
// WHY THIS FILE EXISTS. The rule used to live inline in the dashboard's
// attention loop, and that was the whole implementation — `pad project stale`
// inherited it by filtering the dashboard's attention list, and `pad project
// ready` / `next` did no date handling AT ALL. So a deadline reached the two
// surfaces that report on work and never the surface an agent actually pulls
// from, which is the sharper form of the complaint in GitHub #1010: not "the
// date isn't honored uniformly" but "the date never reaches the recommendation".
//
// Extracting it is what makes "all four surfaces agree" a property of the code
// rather than a thing four call sites happen to do the same way.
//
// WHAT IS DELIBERATELY UNCHANGED: the comparison is still a lexicographic
// string compare against the SERVER'S LOCAL calendar day. That is wrong for a
// multi-timezone deployment and known to be — it is filed as its own item with
// the cloud case stated. Fixing it here would have changed what "overdue"
// means on every existing self-hosted instance inside a change whose subject
// is where the rule LIVES, and a behaviour change smuggled into a refactor is
// the kind nobody reviews.

// overdueDateFields are the field keys that carry a deadline, in report
// priority order.
//
// A LITERAL LIST, not a schema annotation, and that is a decision rather than
// an omission: annotating a FieldDef does not survive an ordinary collection
// edit (the web editor rebuilds each field from an allowlist; CollectionSchema
// has no catch-all), which is exactly why the reminder primitive is a table.
// Convention-by-field-name is the weaker mechanism, but it is the one that
// cannot silently disarm itself.
var overdueDateFields = []string{"due_date", "end_date"}

// overdueToday renders the calendar day deadlines are measured against.
// Server-local, matching the behaviour this preserves.
func overdueToday(now time.Time) string { return now.Format("2006-01-02") }

// itemOverdue reports whether an item has a deadline in the past, and which
// field carried it. Reports at most ONE field per item — the first in
// overdueDateFields order — because an item that is both past its due_date and
// past its end_date is one late item, not two.
//
// Values are compared as strings. ISO-8601 orders lexicographically the same
// way it orders chronologically, so this is correct for `YYYY-MM-DD`, and an
// RFC3339 value (which the `date` field type also admits) sorts after the bare
// day it falls on — so a timestamped value dated TODAY reads as not-yet-late,
// which is the right answer for a due date.
func itemOverdue(fieldsJSON, todayStr string) (field, value string, ok bool) {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return "", "", false
	}
	for _, key := range overdueDateFields {
		v := extractFieldValue(fieldsJSON, key)
		if v == "" {
			continue
		}
		if v < todayStr {
			return key, v, true
		}
	}
	return "", "", false
}

// overdueReason renders the human-facing explanation attached to an overdue
// report ("due date was 2026-08-01"). Shared so the dashboard's attention
// entry and a suggestion's reason cannot drift into two different phrasings of
// the same fact.
func overdueReason(field, value string) string {
	return strings.ReplaceAll(field, "_", " ") + " was " + value
}

// overdueReasonOrEmpty renders the reason only when the item is actually
// overdue, so a caller can fill a struct field unconditionally without
// branching. Returning "" for a not-overdue item keeps the empty string
// meaning "no deadline verdict" rather than "a verdict that rendered blank".
func overdueReasonOrEmpty(field, value string, overdue bool) string {
	if !overdue {
		return ""
	}
	return overdueReason(field, value)
}
