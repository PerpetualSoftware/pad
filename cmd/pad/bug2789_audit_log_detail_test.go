package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// BUG-2789: the audit-log Details column applies the activity display rule to
// a `changes` member and cuts by runes.
func TestAuditLogDetail(t *testing.T) {
	legacy := `{"changes":"status: open → done; implementation_notes: [] → [map[id:note-1 summary:a long legacy blob]]"}`
	if got := auditLogDetail(legacy); strings.Contains(got, "implementation_notes") || !strings.Contains(got, "status: open → done") {
		t.Errorf("legacy notes blob not suppressed: %q", got)
	}
	if got := auditLogDetail(`{"changes":"decision_log: [] → [map[decision:x]]"}`); got != "-" {
		t.Errorf("a changes member emptied by the rule should print -, got %q", got)
	}
	if got := auditLogDetail(`{}`); got != "{}" {
		t.Errorf("metadata without changes must be unchanged, got %q", got)
	}
	if got := auditLogDetail(""); got != "-" {
		t.Errorf("empty metadata: %q", got)
	}
	long := `{"email":"` + strings.Repeat("é", 80) + `"}`
	got := auditLogDetail(long)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 60 {
		t.Errorf("cut must be rune-safe at 60 runes: valid=%v runes=%d", utf8.ValidString(got), utf8.RuneCountInString(got))
	}
}
