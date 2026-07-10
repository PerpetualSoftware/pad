package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/fatih/color"
)

// forcePlainColor disables ANSI so width/content assertions are deterministic
// regardless of whether the test binary's stdout happens to be a TTY.
func forcePlainColor(t *testing.T) {
	t.Helper()
	prev := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = prev })
}

func TestDisplayWidth_StripsANSIAndCountsRunes(t *testing.T) {
	// A literal SGR-wrapped string must measure the same as its plain form.
	colored := "\x1b[1;36mTASK-5\x1b[0m"
	if got := displayWidth(colored); got != 6 {
		t.Errorf("displayWidth(colored) = %d, want 6", got)
	}
	if got := displayWidth("TASK-5"); got != 6 {
		t.Errorf("displayWidth(plain) = %d, want 6", got)
	}
	if displayWidth(colored) != displayWidth("TASK-5") {
		t.Errorf("colored and plain widths differ: %d vs %d",
			displayWidth(colored), displayWidth("TASK-5"))
	}

	// Multi-byte runes and the ellipsis each count as one column.
	if got := displayWidth("café"); got != 4 {
		t.Errorf("displayWidth(café) = %d, want 4", got)
	}
	if got := displayWidth("ab…"); got != 3 {
		t.Errorf("displayWidth(ab…) = %d, want 3", got)
	}
}

func TestRenderItemTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderItemTable(&buf, nil, 120)
	if got := buf.String(); got != "No items found.\n" {
		t.Errorf("empty render = %q, want %q", got, "No items found.\n")
	}
}

func TestRenderItemTable_TruncatesTitleWithinBudget(t *testing.T) {
	forcePlainColor(t)

	items := []models.Item{{
		Title:            strings.Repeat("x", 300),
		Fields:           `{"status":"open","priority":"high"}`,
		CollectionName:   "Tasks",
		CollectionPrefix: "TASK",
		ItemNumber:       intPtr(1),
		UpdatedAt:        time.Now(),
	}}

	const maxWidth = 60
	var buf bytes.Buffer
	renderItemTable(&buf, items, maxWidth)

	out := buf.String()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := displayWidth(line); w > maxWidth {
			t.Errorf("line exceeds maxWidth %d (got %d): %q", maxWidth, w, line)
		}
	}
	// The long title had to be cut, so an ellipsis must appear.
	if !strings.Contains(out, "…") {
		t.Errorf("expected truncation ellipsis in output, got:\n%s", out)
	}
}

func TestRenderItemTable_NeverTruncatesRef(t *testing.T) {
	forcePlainColor(t)

	items := []models.Item{{
		Title:            strings.Repeat("y", 200),
		CollectionName:   "Tasks",
		CollectionPrefix: "TASK",
		ItemNumber:       intPtr(123),
		UpdatedAt:        time.Now(),
	}}

	var buf bytes.Buffer
	renderItemTable(&buf, items, 30) // deliberately cramped
	if !strings.Contains(buf.String(), "TASK-123") {
		t.Errorf("ref TASK-123 must survive even a cramped width, got:\n%s", buf.String())
	}
}

func TestRenderItemTable_StripsANSIFromTitle(t *testing.T) {
	forcePlainColor(t)

	// A title carrying raw SGR escapes must not slice mid-escape or throw off
	// the width budget: the escapes are stripped before truncation.
	items := []models.Item{{
		Title:            "\x1b[31mRED\x1b[0m" + strings.Repeat("z", 200),
		CollectionName:   "Tasks",
		CollectionPrefix: "TASK",
		ItemNumber:       intPtr(1),
		UpdatedAt:        time.Now(),
	}}

	const maxWidth = 60
	var buf bytes.Buffer
	renderItemTable(&buf, items, maxWidth)
	out := buf.String()

	if strings.Contains(out, "\x1b[31m") || strings.Contains(out, "\x1b[0m") {
		t.Errorf("expected embedded SGR escapes to be stripped from the title, got: %q", out)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := displayWidth(line); w > maxWidth {
			t.Errorf("line exceeds maxWidth %d (got %d): %q", maxWidth, w, line)
		}
	}
}

func TestRenderItemTable_StatusAndPriorityFromFields(t *testing.T) {
	forcePlainColor(t)

	items := []models.Item{
		{
			Title:            "with fields",
			Fields:           `{"status":"open","priority":"high"}`,
			CollectionName:   "Tasks",
			CollectionPrefix: "TASK",
			ItemNumber:       intPtr(1),
			UpdatedAt:        time.Now(),
		},
		{
			Title:            "no fields",
			Fields:           "{}",
			CollectionName:   "Docs",
			CollectionPrefix: "DOC",
			ItemNumber:       intPtr(2),
			UpdatedAt:        time.Now(),
		},
	}

	var buf bytes.Buffer
	renderItemTable(&buf, items, 120)
	out := buf.String()

	for _, want := range []string{"open", "high", "STATUS", "PRIORITY", "TASK-1", "DOC-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
	// The BY column is gone.
	if strings.Contains(out, "BY") {
		t.Errorf("BY column should have been dropped, got:\n%s", out)
	}
	// The fieldless row shows an em-dash placeholder for status/priority.
	if !strings.Contains(out, "—") {
		t.Errorf("expected em-dash placeholder for missing status/priority, got:\n%s", out)
	}
}

func TestItemStatusPriority(t *testing.T) {
	cases := []struct {
		name         string
		fields       string
		wantS, wantP string
	}{
		{"both", `{"status":"open","priority":"high"}`, "open", "high"},
		{"empty-object", "{}", "", ""},
		{"empty-string", "", "", ""},
		{"malformed", "{not json", "", ""},
		{"non-string", `{"status":3,"priority":true}`, "", ""},
		{"status-only", `{"status":"done"}`, "done", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotS, gotP := itemStatusPriority(tc.fields)
			if gotS != tc.wantS || gotP != tc.wantP {
				t.Errorf("itemStatusPriority(%q) = (%q, %q), want (%q, %q)",
					tc.fields, gotS, gotP, tc.wantS, tc.wantP)
			}
		})
	}
}
