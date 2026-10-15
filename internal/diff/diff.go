// Package diff provides compact text diffing for version storage.
// It uses the Myers diff algorithm via go-diff to create and apply patches.
package diff

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	difflib "github.com/sergi/go-diff/diffmatchpatch"
)

var dmp = difflib.New()

// CreatePatch computes a compact patch that transforms oldText into newText.
// The patch can be applied with ApplyPatch to recover newText from oldText.
// Both texts must be valid UTF-8 (BUG-3839; see ReversePatch).
func CreatePatch(oldText, newText string) string {
	diffs := dmp.DiffMain(oldText, newText, true)
	patches := dmp.PatchMake(oldText, diffs)
	return dmp.PatchToText(patches)
}

// ApplyPatch applies a patch to baseText to produce the target text.
// Returns an error if the patch cannot be applied cleanly, or if the base or
// the patch's text is not valid UTF-8: go-diff slices such text at offsets it
// derives from runes, and panics or builds the wrong text (BUG-3839).
//
// go-diff also panics on valid text, when a patch is applied over a base it
// was not made from and the base holds multi-byte runes (about 1.5% of such
// pairs in the BUG-3839 measurement). The version walk does exactly that
// after it skips a row, so that panic is an error here too.
func ApplyPatch(baseText, patchText string) (result string, err error) {
	if patchText == "" {
		return baseText, nil
	}
	if !utf8.ValidString(baseText) {
		return "", fmt.Errorf("base text is not valid UTF-8")
	}
	if !patchTextValidUTF8(patchText) {
		return "", fmt.Errorf("patch text is not valid UTF-8")
	}
	patches, err := dmp.PatchFromText(patchText)
	if err != nil {
		return "", fmt.Errorf("parse patch: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			result, err = "", fmt.Errorf("apply patch: %v", r)
		}
	}()
	result, applied := dmp.PatchApply(patches, baseText)
	// Check that all hunks applied
	for i, ok := range applied {
		if !ok {
			return "", fmt.Errorf("patch hunk %d failed to apply", i)
		}
	}
	return result, nil
}

// patchTextValidUTF8 reports whether every text line of a patch decodes to
// valid UTF-8. A line is escaped, so "%FF" is the byte 0xFF once parsed; this
// decodes each line the way PatchFromText does, whose result is unexported.
func patchTextValidUTF8(patchText string) bool {
	for _, line := range strings.Split(patchText, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case ' ', '-', '+':
			// PatchFromText ignores the error too: a malformed line decodes
			// to "", which is valid.
			text, _ := url.QueryUnescape(strings.ReplaceAll(line[1:], "+", "%2b"))
			if !utf8.ValidString(text) {
				return false
			}
		}
	}
	return true
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
//
// Both texts must be valid UTF-8, as for CreatePatch; production code calls
// ReversePatch, which checks (BUG-3839, TestNoUncheckedPatchCallers).
func CreateReversePatch(oldText, newText string) string {
	return CreatePatch(newText, oldText)
}

// ReversePatch is CreateReversePatch for text that may not be valid UTF-8
// (BUG-3839). go-diff works on runes and slices the input at byte offsets it
// derives from them, so invalid UTF-8 near an edit either panics (slice
// bounds out of range) or yields a patch that does not reproduce oldText:
// "hello world" -> "hello \xff world" reverse-applied to "hello d". ok is
// false when either side is not valid UTF-8, and the caller stores the full
// body instead. Never sanitize and diff: the version would then record a body
// it does not hold.
func ReversePatch(oldText, newText string) (patch string, ok bool) {
	if !utf8.ValidString(oldText) || !utf8.ValidString(newText) {
		return "", false
	}
	return CreatePatch(newText, oldText), true
}

// FormatDiffSummary returns a human-readable summary of changes. Its "lines"
// are character-diff CHUNKS (a one-character edit reads +1/-1); LineCounts is
// the line-level count the version rows store (PLAN-2348 U2).
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

// LineCounts returns how many lines a line-level diff of oldText → newText
// adds and removes (PLAN-2348 U2). It counts the way the web DiffView renders
// jsdiff's diffLines: each changed run is split on newlines after dropping one
// trailing newline, so a run "a\nb\n" is two lines. Both sides are Myers
// line diffs; diffmatchpatch's speed-up heuristics can choose a different,
// equally valid split on unusual inputs, in which case a count can differ from
// the rendered diff by the lines those splits trade.
func LineCounts(oldText, newText string) (added, removed int) {
	a, b, lines := dmp.DiffLinesToChars(oldText, newText)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)
	for _, d := range diffs {
		n := strings.Count(strings.TrimSuffix(d.Text, "\n"), "\n") + 1
		switch d.Type {
		case difflib.DiffInsert:
			added += n
		case difflib.DiffDelete:
			removed += n
		}
	}
	return added, removed
}
