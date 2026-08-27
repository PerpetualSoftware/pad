package models

import (
	"regexp"
	"strings"
	"testing"
)

// The two mechanisms a stored wiki-link bracket passes through, mirrored from
// web/src/lib/utils/markdown.ts so this test can derive the validator's rule
// from them instead of from an opinion about which characters look dangerous.
//
// Duplicated rather than approximated. If the TypeScript changes, these should
// start disagreeing with ValidateDocumentTitle — that is the signal wanted,
// not a nuisance.
var (
	// markdown.ts:327 — shared by renderMarkdown and wikiLinksToMarkdown.
	storedWikiLinkBracket = regexp.MustCompile(`\[\[((?:\\.|[^\]\\])+)\]\]`)
	// markdown.ts:753 — unescapeWikiBody, applied before title comparison.
	wikiBodyEscape = regexp.MustCompile(`\\(\\|\]|\|)`)
)

// roundTrips reports whether emitting `[[title]]` the way links.ReplaceTitle
// does — plain concatenation, no escaping — produces a bracket that reads back
// as exactly this title.
//
// Both layers, because either alone gives a wrong answer. The grammar alone
// accepts `A\\B` (a valid escape pair) which the unescaper then turns into
// `A\B`, a different title. The unescaper alone accepts `A]B`, which the
// grammar cuts short.
func roundTrips(title string) bool {
	emitted := "[[" + title + "]]"
	m := storedWikiLinkBracket.FindStringSubmatch(emitted)
	if m == nil || m[0] != emitted {
		// No match, or a match over a PREFIX of the emission — the latter is
		// BUG-2796's early-termination defect, where `[[A]] [[A]]` matched
		// twice and neither match was the renamed document.
		return false
	}
	return wikiBodyEscape.ReplaceAllString(m[1], "$1") == title
}

// TestDocumentTitleValidationMatchesTheRoundTripProperty is the justification
// for the rule, in executable form: the validator must reject a title if and
// only if that title's links would not survive being rewritten to it.
//
// Both directions are asserted, because either alone permits a wrong answer.
// Without the reject leg, a validator that accepted everything passes.
// Without the accept leg, a validator that rejected `[`, `|` or a lone `\`
// passes — and that is not hypothetical: the first version of this fix banned
// `]`, `\` and `|` as a character class, and this test refuted two thirds of
// it. `|` in particular is a title shape resolveWikiBody contains a dedicated
// branch to support.
func TestDocumentTitleValidationMatchesTheRoundTripProperty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		title string
	}{
		// Must be REJECTED — each breaks the round trip by a different
		// mechanism.
		{"bug-2796 repro", `A]] [[A`},
		{"single close bracket", `Alpha]Beta`},
		{"trailing backslash", `Alpha\`},
		{"escaped backslash", `Alpha\\Beta`},
		{"escaped pipe", `Alpha\|Beta`},

		// Must be ACCEPTED — each looks like wiki-link syntax and none of
		// them breaks anything.
		{"open bracket", `Alpha[Beta`},
		{"double open bracket", `Alpha[[Beta`},
		{"literal pipe", `Alpha|Beta`},
		{"lone backslash", `Alpha\Beta`},
		{"collection-qualified shape", `collection/Title`},
		{"colons", `Ratio 1:2`},
		{"ordinary punctuation", `Title (draft) — v2`},
		{"non-latin", `Ünïcödé título`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantAccept := roundTrips(tc.title)
			msg := ValidateDocumentTitle(tc.title)
			gotAccept := msg == ""
			if gotAccept != wantAccept {
				t.Errorf("title %q: round-trips=%v but validator accepts=%v (message: %q)\n"+
					"the validator and the renderers must agree — a rejected title that renders "+
					"correctly is a refusal of valid input; an accepted title that does not is BUG-2796",
					tc.title, wantAccept, gotAccept, msg)
			}
		})
	}
}

// TestDocumentTitleLengthBoundCountsRunesNotBytes pins the bound at 255
// CHARACTERS, which is what the ruling says and what a UI counter shows.
//
// The multibyte legs are the counterfactual: 255 four-byte runes are 1020
// bytes, so an implementation that reached for len() would reject them. That
// implementation would be wrong in a user-visible way — an ordinary
// 255-character title in a non-Latin script refused — while every ASCII-only
// test stayed green.
func TestDocumentTitleLengthBoundCountsRunesNotBytes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		title  string
		accept bool
	}{
		{"at the bound, ascii", strings.Repeat("a", MaxDocumentTitleRunes), true},
		{"one over, ascii", strings.Repeat("a", MaxDocumentTitleRunes+1), false},
		{"at the bound, 2-byte runes", strings.Repeat("é", MaxDocumentTitleRunes), true},
		{"at the bound, 4-byte runes", strings.Repeat("𝄞", MaxDocumentTitleRunes), true},
		{"one over, 4-byte runes", strings.Repeat("𝄞", MaxDocumentTitleRunes+1), false},
		{"empty", "", false},
	} {
		msg := ValidateDocumentTitle(tc.title)
		if tc.accept && msg != "" {
			t.Errorf("%s: rejected (%s), want accepted", tc.name, msg)
		}
		if !tc.accept && msg == "" {
			t.Errorf("%s: accepted, want rejected", tc.name)
		}
	}
}
