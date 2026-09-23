package attachments

import (
	"net/http"
	"sort"
	"testing"
)

// BUG-2841: an allowlisted text type that no upload can produce. Before
// BUG-2963 F5 (#1309) every text-family entry but text/plain was dead by
// construction: ValidateUpload returned the SNIFFED type, and the stdlib sniffs
// any prose as text/plain, so text/markdown sat on the allowlist while a real
// .md upload stored text/plain. Tests that hand-set the MIME passed; a browser
// found it.
//
// This test asks the question that finds that shape, for EVERY entry, read
// from the table rather than listed by hand, so an entry added later is
// covered without anyone remembering to add it: which real upload produces it?
//
// It also pins the property that makes the refinement safe (lead ruling on
// BUG-2841, option a): choosing a text type from the extension NEVER GAINS
// inline serving. text/plain is the only text type in inlineSafe, so every
// refinement moves a file from inline toward attachment, never the reverse.
func TestTextFamilyEntriesAreReachableAndNeverGainInline(t *testing.T) {
	// Bytes the stdlib calls text/plain and nothing finer: the input under
	// which the extension, and only the extension, can choose.
	body := []byte("plain words, nothing more\n")
	if got := NormalizeMIME(http.DetectContentType(body)); got != "text/plain" {
		t.Fatalf("precondition: the sample must sniff as text/plain, got %q", got)
	}

	var exts []string
	for ext := range extMIMEMap {
		exts = append(exts, ext)
	}
	sort.Strings(exts)

	checked := 0
	for _, ext := range exts {
		mapped, ok := LookupMIME(NormalizeMIME(extMIMEMap[ext]))
		if !ok || mapped.Category != CategoryText {
			continue // blocked, or not a text type: not this test's population
		}
		checked++
		got, code, err := ValidateUpload(body, "f"+ext)
		if err != nil {
			t.Errorf("%s: plain text under its own extension was refused (%s): %v", ext, code, err)
			continue
		}
		// Reachable: the extension's own entry is what lands in the row.
		if got.MIME != mapped.MIME {
			t.Errorf("%s maps to %s, but an upload of it stores %s: an allowlisted entry no upload can produce (BUG-2841)",
				ext, mapped.MIME, got.MIME)
		}
		// Never gains inline: only text/plain itself may be served inline.
		if got.MIME != "text/plain" && got.ServeInline() {
			t.Errorf("%s refines text/plain to %s, which is served INLINE: the extension widened what renders in the tab",
				ext, got.MIME)
		}
	}
	// The population is the text family, and it must not be empty or shrink
	// unnoticed: a filter that matched nothing would pass every check above.
	if checked < 10 {
		t.Fatalf("only %d text-family extensions checked; expected the full family (>= 10)", checked)
	}
}
