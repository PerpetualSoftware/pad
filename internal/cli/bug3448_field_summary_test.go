package cli

import (
	"strings"
	"testing"
)

// BUG-3448: the one-line field summary (`pad item update`'s echo) printed a
// json field in Go's map syntax. A structured value now summarises as its
// size, which keeps the line one line.
func TestFormatFieldSummarySummarisesStructuredValues(t *testing.T) {
	got := FormatFieldSummary(`{"status":"open","arguments":[{"name":"a"},{"name":"b"}],"meta":{"k":1}}`)
	if strings.Contains(got, "map[") || strings.Contains(got, "[map") {
		t.Fatalf("a structured value printed in Go map syntax: %q", got)
	}
	for _, want := range []string{"arguments: (2 entries)", "meta: (1 key)"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
	if !strings.Contains(got, "open") {
		t.Errorf("control: the scalar status is missing from %q", got)
	}
}

// Codex r1: a number above 2^53 keeps its digits in the summary too.
func TestFormatFieldSummaryKeepsLargeNumbers(t *testing.T) {
	if got := FormatFieldSummary(`{"budget":9007199254740993}`); !strings.Contains(got, "9007199254740993") {
		t.Errorf("summary %q lost the number's digits", got)
	}
}
