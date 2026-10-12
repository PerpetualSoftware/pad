// Package bodypatch applies content-anchored edits to an item body
// (PLAN-3578): each edit names text that must occur EXACTLY ONCE in the body
// at the moment it applies, and replaces it. Anchors are text, never line
// numbers: the body is markdown serialized from a collaborative document, so
// line N at read time need not be line N at write time. A line hint may only
// choose among several exact matches; it never stands in for one.
package bodypatch

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
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
	original := body
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
	if body == original {
		return "", ErrNoChange
	}
	return body, nil
}

// ErrNoChange: the edits, applied in order, leave the body exactly as it was
// (retro CONVE-735 finding on #2055). A patch is a version; one that changes
// nothing is refused rather than advancing seq with no version behind it.
var ErrNoChange = errors.New("the edits change nothing")

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

// ContextChars bounds the body context kept on each side of an applied edit
// (PLAN-3578 step 3, lead ruling: about 40, clipped at line ends).
const ContextChars = 40

// AppliedEdit is an edit as it landed: what was sent, plus the body text on
// either side of the replacement right after it applied, so an undo can
// anchor on text even when the edit deleted its Old.
type AppliedEdit struct {
	Old       string `json:"old"`
	New       string `json:"new"`
	CtxBefore string `json:"ctx_before"`
	CtxAfter  string `json:"ctx_after"`
}

// ApplyWithContext is Apply, also returning each edit with its context. The
// context of edit i is read from the body as it stood right after edit i,
// which is exactly the body an undo of edit i meets when the edits after it
// have been undone first, so Invert's reverse order lines up.
func ApplyWithContext(body string, edits []Edit) (string, []AppliedEdit, error) {
	if _, err := Apply(body, edits); err != nil {
		return "", nil, err
	}
	out := make([]AppliedEdit, 0, len(edits))
	for _, e := range edits {
		at := matches(body, e.Old)
		if len(at) > 1 {
			for _, p := range at {
				if lineOf(body, p) == e.LineHint {
					at = []int{p}
					break
				}
			}
		}
		p := at[0]
		body = body[:p] + e.New + body[p+len(e.Old):]
		end := p + len(e.New)
		cb, ca := ctxBefore(body, p, true), ctxAfter(body, end, true)
		if cb+e.New+ca == "" {
			// The edit emptied its line (retro CONVE-735 finding on #2058):
			// line-clipped context is empty on both sides, which would leave
			// the undo no anchor at all. Reach across the line breaks instead.
			cb, ca = ctxBefore(body, p, false), ctxAfter(body, end, false)
		}
		out = append(out, AppliedEdit{Old: e.Old, New: e.New, CtxBefore: cb, CtxAfter: ca})
	}
	return body, out, nil
}

// ctxBefore is up to ContextChars bytes before p, not crossing a newline when
// clip is set, and never splitting a UTF-8 sequence.
func ctxBefore(s string, p int, clip bool) string {
	from := p - ContextChars
	if from < 0 {
		from = 0
	}
	if nl := strings.LastIndexByte(s[from:p], '\n'); clip && nl >= 0 {
		from += nl + 1
	}
	for from < p && !utf8.RuneStart(s[from]) {
		from++
	}
	return s[from:p]
}

// ctxAfter is up to ContextChars bytes from p, stopping before a newline when
// clip is set.
func ctxAfter(s string, p int, clip bool) string {
	to := p + ContextChars
	if to > len(s) {
		to = len(s)
	}
	if nl := strings.IndexByte(s[p:to], '\n'); clip && nl >= 0 {
		to = p + nl
	}
	for to > p && to < len(s) && !utf8.RuneStart(s[to]) {
		to--
	}
	return s[p:to]
}

// Invert turns applied edits into the patch that undoes them: each anchored
// on its context around what it wrote, replaced by its context around what it
// removed, in reverse order. The ordinary rules then apply: an anchor that no
// longer occurs exactly once (the text was edited since) refuses the undo and
// writes nothing, so an undo never reverts someone else's later change.
func Invert(applied []AppliedEdit) []Edit {
	out := make([]Edit, 0, len(applied))
	for i := len(applied) - 1; i >= 0; i-- {
		a := applied[i]
		out = append(out, Edit{Old: a.CtxBefore + a.New + a.CtxAfter, New: a.CtxBefore + a.Old + a.CtxAfter})
	}
	return out
}
