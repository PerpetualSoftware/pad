package diff

import (
	"strings"
	"testing"
)

func TestCreateAndApplyPatch(t *testing.T) {
	old := "Hello world\nThis is a test\nThird line"
	new := "Hello world\nThis is modified\nThird line\nFourth line"

	patch := CreatePatch(old, new)
	if patch == "" {
		t.Fatal("expected non-empty patch")
	}

	result, err := ApplyPatch(old, patch)
	if err != nil {
		t.Fatalf("ApplyPatch error: %v", err)
	}
	if result != new {
		t.Errorf("expected %q, got %q", new, result)
	}
}

func TestReversePatch(t *testing.T) {
	old := "# Document\n\nOriginal content here."
	new := "# Document\n\nUpdated content here.\n\nNew section added."

	patch := CreateReversePatch(old, new)
	if patch == "" {
		t.Fatal("expected non-empty reverse patch")
	}

	// Applying reverse patch to new content should give old content
	result, err := ApplyPatch(new, patch)
	if err != nil {
		t.Fatalf("ApplyPatch error: %v", err)
	}
	if result != old {
		t.Errorf("expected %q, got %q", old, result)
	}
}

func TestEmptyPatch(t *testing.T) {
	content := "Hello world"
	result, err := ApplyPatch(content, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != content {
		t.Errorf("expected %q, got %q", content, result)
	}
}

func TestIdenticalContent(t *testing.T) {
	content := "Hello world"
	patch := CreatePatch(content, content)
	if patch != "" {
		t.Errorf("expected empty patch for identical content, got %q", patch)
	}
}

func TestIsDiffSmaller(t *testing.T) {
	// Small change to large doc — diff should be smaller
	large := strings.Repeat("Line of text\n", 100)
	modified := large + "Added line\n"
	patch := CreatePatch(large, modified)
	if !IsDiffSmaller(patch, large) {
		t.Errorf("expected diff (%d bytes) to be smaller than full content (%d bytes)", len(patch), len(large))
	}

	// Near-complete rewrite — diff should NOT be smaller
	completely := strings.Repeat("Totally different\n", 100)
	patch2 := CreatePatch(large, completely)
	if IsDiffSmaller(patch2, large) {
		t.Errorf("expected diff (%d bytes) to NOT be smaller for complete rewrite (%d bytes)", len(patch2), len(large))
	}
}

func TestIsDiffSmallerEmptyContent(t *testing.T) {
	if IsDiffSmaller("some patch", "") {
		t.Error("expected false for empty content")
	}
}

func TestChainedReversePatches(t *testing.T) {
	// Simulate version chain: v1 → v2 → v3 (current)
	v1 := "Version 1 content"
	v2 := "Version 2 content with changes"
	v3 := "Version 3 content with more changes"

	// Store reverse patches (from newer → older)
	patch3to2 := CreateReversePatch(v2, v3) // patch to go v3 → v2
	patch2to1 := CreateReversePatch(v1, v2) // patch to go v2 → v1

	// Reconstruct v2 from v3
	got2, err := ApplyPatch(v3, patch3to2)
	if err != nil {
		t.Fatalf("reconstruct v2: %v", err)
	}
	if got2 != v2 {
		t.Errorf("v2 reconstruction: expected %q, got %q", v2, got2)
	}

	// Reconstruct v1 from v2
	got1, err := ApplyPatch(got2, patch2to1)
	if err != nil {
		t.Fatalf("reconstruct v1: %v", err)
	}
	if got1 != v1 {
		t.Errorf("v1 reconstruction: expected %q, got %q", v1, got1)
	}
}
