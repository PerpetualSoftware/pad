package cli

import (
	"strings"
	"testing"
)

func TestFormatLineDiffMarksChangesAndElidesDistantContext(t *testing.T) {
	var old, next []string
	for i := 0; i < 20; i++ {
		old = append(old, "line "+string(rune('a'+i)))
	}
	next = append(next, old...)
	next[10] = "line k, changed"
	got := FormatLineDiff(strings.Join(old, "\n")+"\n", strings.Join(next, "\n")+"\n", 2)
	for _, want := range []string{"-line k\n", "+line k, changed\n", " line i\n", " line m\n", "  …\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, " line a\n") {
		t.Errorf("a line far from the change was not elided:\n%s", got)
	}
	if FormatLineDiff("same\n", "same\n", 3) != "" {
		t.Error("equal texts produced a diff")
	}
}

func TestBuiltinFieldChanges(t *testing.T) {
	changes := BuiltinFieldChanges(
		map[string]any{"trigger": "on-release", "n": 1e3, "status": "deprecated", "legacy": "x"},
		map[string]any{"trigger": "manual", "n": 1000.0, "status": "active"},
		map[string]any{"legacy": "x"},
	)
	if len(changes) != 2 {
		t.Fatalf("changes %+v, want trigger and the dropped legacy only", changes)
	}
	if changes[0].Key != "legacy" || changes[0].HasLib {
		t.Errorf("dropped field not reported as removed: %+v", changes[0])
	}
	if changes[1].Key != "trigger" || changes[1].Current != "on-release" || changes[1].Library != "manual" {
		t.Errorf("trigger change wrong: %+v", changes[1])
	}
}

// codex r1 (P3): a change to the final newline alone is visible.
func TestFormatLineDiffShowsAnEndOfFileNewlineChange(t *testing.T) {
	got := FormatLineDiff("a\n", "a", 3)
	if !strings.Contains(got, "No newline at end of file") {
		t.Fatalf("newline change hidden:\n%q", got)
	}
}
