package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadWorkspaceContextInputFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.json")
	if err := os.WriteFile(path, []byte(`{"commands":{"build":"make install"},"assumptions":["Tasks should be PR-sized"]}`), 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	context, err := readWorkspaceContextInput(path, false)
	if err != nil {
		t.Fatalf("readWorkspaceContextInput error: %v", err)
	}
	if context.Commands == nil || context.Commands.Build != "make install" {
		t.Fatalf("expected build command from file, got %#v", context.Commands)
	}
	if len(context.Assumptions) != 1 {
		t.Fatalf("expected assumptions from file, got %#v", context.Assumptions)
	}
}

func TestReadWorkspaceContextInputRequiresSingleSource(t *testing.T) {
	if _, err := readWorkspaceContextInput("", false); err == nil {
		t.Fatal("expected missing input source to fail")
	}
	if _, err := readWorkspaceContextInput("context.json", true); err == nil {
		t.Fatal("expected conflicting input sources to fail")
	}
}
