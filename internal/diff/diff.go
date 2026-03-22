// Package diff provides compact text diffing for version storage.
// It uses the Myers diff algorithm via go-diff to create and apply patches.
package diff

import (
	"fmt"
	"strings"

	difflib "github.com/sergi/go-diff/diffmatchpatch"
)

var dmp = difflib.New()

// CreatePatch computes a compact patch that transforms oldText into newText.
// The patch can be applied with ApplyPatch to recover newText from oldText.
func CreatePatch(oldText, newText string) string {
	diffs := dmp.DiffMain(oldText, newText, true)
	patches := dmp.PatchMake(oldText, diffs)
	return dmp.PatchToText(patches)
}

// ApplyPatch applies a patch to baseText to produce the target text.
// Returns an error if the patch cannot be applied cleanly.
func ApplyPatch(baseText, patchText string) (string, error) {
	if patchText == "" {
		return baseText, nil
	}
	patches, err := dmp.PatchFromText(patchText)
	if err != nil {
		return "", fmt.Errorf("parse patch: %w", err)
	}
	result, applied := dmp.PatchApply(patches, baseText)
	// Check that all hunks applied
	for i, ok := range applied {
		if !ok {
			return "", fmt.Errorf("patch hunk %d failed to apply", i)
		}
	}
	return result, nil
}

// IsDiffSmaller returns true if storing a diff would be meaningfully smaller
// than storing the full content. Returns false for near-complete rewrites
// where the diff is >= 80% of the full content size.
func IsDiffSmaller(patch, fullContent string) bool {
	if len(fullContent) == 0 {
		return false
	}
	return len(patch) < (len(fullContent) * 80 / 100)
}

// CreateReversePatch computes a patch that transforms newText back into oldText.
// Used for reverse-diff version storage: current doc has latest content,
// versions store patches to go backwards.
func CreateReversePatch(oldText, newText string) string {
	return CreatePatch(newText, oldText)
}

// FormatDiffSummary returns a human-readable summary of changes.
func FormatDiffSummary(oldText, newText string) string {
	oldLines := strings.Count(oldText, "\n")
	newLines := strings.Count(newText, "\n")
	added := 0
	removed := 0
	diffs := dmp.DiffMain(oldText, newText, false)
	for _, d := range diffs {
		switch d.Type {
		case difflib.DiffInsert:
			added += strings.Count(d.Text, "\n") + 1
		case difflib.DiffDelete:
			removed += strings.Count(d.Text, "\n") + 1
		}
	}
	_ = oldLines
	_ = newLines
	return fmt.Sprintf("+%d/-%d lines", added, removed)
}
