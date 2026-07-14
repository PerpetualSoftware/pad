package main

import (
	"strings"
	"testing"
)

func TestItemListParentAndUnparentedAreMutuallyExclusive(t *testing.T) {
	cmd := listCmd()
	cmd.SetArgs([]string{"--parent", "PLAN-1", "--unparented"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected --parent + --unparented to fail")
	}
	if !strings.Contains(err.Error(), "[parent unparented]") {
		t.Fatalf("unexpected error: %v", err)
	}
}
