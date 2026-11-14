package main

import "testing"

// TestPickCloseValueOptionNamedAuto: a delivering close that is itself named
// "auto" is chosen by --close-item=auto rather than read as the bare flag.
func TestPickCloseValueOptionNamedAuto(t *testing.T) {
	d := itemStillOpenDetails{DoneField: "status", CloseOptions: []string{"done", "Auto"}}
	got, err := pickCloseValue(closeItemAuto, "TASK-1", d)
	if err != nil || got != "Auto" {
		t.Fatalf("pickCloseValue(auto) = %q, %v; want %q, nil", got, err, "Auto")
	}
	// The bare flag with several options and none named auto still refuses.
	d.CloseOptions = []string{"done", "fixed"}
	if got, err := pickCloseValue(closeItemAuto, "TASK-1", d); err == nil {
		t.Fatalf("pickCloseValue(auto) with two options = %q, nil; want a refusal", got)
	}
	// The bare flag with one option uses it.
	d.CloseOptions = []string{"done"}
	if got, err := pickCloseValue(closeItemAuto, "TASK-1", d); err != nil || got != "done" {
		t.Fatalf("pickCloseValue(auto) with one option = %q, %v; want done", got, err)
	}
}
