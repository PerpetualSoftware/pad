package main

import "testing"

// TestPickCloseValueSentinel: the bare flag never collides with a real option
// value, and an explicit --close-item=auto chooses an option named Auto.
func TestPickCloseValueSentinel(t *testing.T) {
	d := itemStillOpenDetails{DoneField: "status", CloseOptions: []string{"done", "Auto"}}
	if got, err := pickCloseValue("auto", "TASK-1", d); err != nil || got != "Auto" {
		t.Fatalf("pickCloseValue(auto) = %q, %v; want Auto, nil", got, err)
	}
	// The bare flag with two options is ambiguous, even when one is named Auto.
	if got, err := pickCloseValue(closeItemAuto, "TASK-1", d); err == nil {
		t.Fatalf("bare --close-item with two options = %q, nil; want a refusal", got)
	}
	// The bare flag with one option uses it.
	d.CloseOptions = []string{"done"}
	if got, err := pickCloseValue(closeItemAuto, "TASK-1", d); err != nil || got != "done" {
		t.Fatalf("bare --close-item with one option = %q, %v; want done", got, err)
	}
}
