// Package bodypatch applies content-anchored edits to an item body
// (PLAN-3578): each edit names text that must occur EXACTLY ONCE in the body
// at the moment it applies, and replaces it. Anchors are text, never line
// numbers: the body is markdown serialized from a collaborative document, so
// line N at read time need not be line N at write time. A line hint may only
// choose among several exact matches; it never stands in for one.
package bodypatch

import (
	"fmt"
	"strings"
)

// Edit is one replacement: Old must occur exactly once (or exactly once on
// LineHint's line when it occurs more than once), and becomes New.
type Edit struct {
	Old string `json:"old"`
	New string `json:"new"`
	// LineHint, when > 0, is the 1-based line on which an ambiguous Old
	// starts. Ignored when Old occurs once.
	LineHint int `json:"line_hint,omitempty"`
}

// MaxEdits bounds one patch.
const MaxEdits = 100

// EmptyAnchorError refuses an edit whose Old is empty: an empty anchor
// matches everywhere, so it says nothing about where the edit goes.
type EmptyAnchorError struct{ Index int }

func (e *EmptyAnchorError) Error() string {
	return fmt.Sprintf("edit %d: old is empty; an anchor must name text in the body", e.Index)
}

// AnchorMissingError: Old does not occur in the body as it stands when the
// edit applies (after the edits before it).
type AnchorMissingError struct{ Index int }

func (e *AnchorMissingError) Error() string {
	return fmt.Sprintf("edit %d: old text not found in the current body", e.Index)
}

// AnchorAmbiguousError: Old occurs more than once and no line hint picks
// exactly one. Lines are the 1-based start lines of every match.
type AnchorAmbiguousError struct {
	Index int
	Lines []int
}

func (e *AnchorAmbiguousError) Error() string {
	return fmt.Sprintf("edit %d: old text occurs %d times (lines %v); quote more context, or pass line_hint", e.Index, len(e.Lines), e.Lines)
}

// Apply runs edits in order against body, each against the result of the
// ones before it, and returns the new body. All or nothing: the first edit
// that cannot apply returns its error and no body.
func Apply(body string, edits []Edit) (string, error) {
	if len(edits) == 0 {
		return "", fmt.Errorf("no edits")
	}
	if len(edits) > MaxEdits {
		return "", fmt.Errorf("%d edits; at most %d per patch", len(edits), MaxEdits)
	}
	for i, e := range edits {
		if e.Old == "" {
			return "", &EmptyAnchorError{Index: i}
		}
	}
	for i, e := range edits {
		at := matches(body, e.Old)
		switch {
		case len(at) == 0:
			return "", &AnchorMissingError{Index: i}
		case len(at) > 1:
			pick := -1
			if e.LineHint > 0 {
				for _, p := range at {
					if lineOf(body, p) == e.LineHint {
						if pick >= 0 {
							pick = -2 // two matches start on the hinted line
							break
						}
						pick = p
					}
				}
			}
			if pick < 0 {
				lines := make([]int, len(at))
				for k, p := range at {
					lines[k] = lineOf(body, p)
				}
				return "", &AnchorAmbiguousError{Index: i, Lines: lines}
			}
			at = []int{pick}
		}
		body = body[:at[0]] + e.New + body[at[0]+len(e.Old):]
	}
	return body, nil
}

// matches returns every start offset of sub in s, overlapping ones included:
// "aa" occurs twice in "aaa", and a uniqueness check that counted it once
// would let an edit land on whichever one a scan happened to find first.
func matches(s, sub string) []int {
	var out []int
	for from := 0; from <= len(s)-len(sub); {
		k := strings.Index(s[from:], sub)
		if k < 0 {
			break
		}
		out = append(out, from+k)
		from += k + 1
	}
	return out
}

// lineOf is the 1-based line on which byte offset p lies.
func lineOf(s string, p int) int {
	return strings.Count(s[:p], "\n") + 1
}
