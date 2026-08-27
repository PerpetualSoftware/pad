package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2798. A document rename rewrites `[[oldTitle]]` → `[[newTitle]]` in
// every linking document, and the cascade holds every rewritten body in memory
// before it writes any of them. Neither the title length nor the number of
// linking documents was bounded, so one rename could project more output than
// the process could hold — measured at 20,000x on the filing, and still 51.8x
// per document after the title bound alone.
//
// The title bound (models.MaxDocumentTitleRunes) is the cheap door. This file
// covers the wall: the cascade refuses when its projected TOTAL exceeds
// MaxRenameCascadeProjectedBytes.

// linkerBody returns a body containing exactly n `[[A]]` occurrences, and the
// number of bytes renaming "A" to a title of length newLen would project for
// it: len(content) + occurrences * (len(new) - len(old)).
//
// Exact, not an estimate — strings.Replace substitutes every non-overlapping
// occurrence, so this is the output size to the byte.
func linkerBody(n, newLen int) (string, int) {
	body := strings.Repeat("[[A]]", n)
	return body, len(body) + n*(newLen-1)
}

// TestRenameCascade_RefusesOnProjectedTOTAL_NotPerDocument is the load-bearing
// test, and its shape is the finding it encodes: a per-document cap would not
// close this bug.
//
// Every linking document here projects comfortably UNDER the cap on its own.
// Only the total is over. A guard that tested each document in isolation would
// admit all three, allocate the sum, and pass a test that merely asserted "a
// huge single document is refused" — which is why this test asserts the
// per-document figure is under the cap as a PRECONDITION rather than trusting
// the constants to stay where they are.
func TestRenameCascade_RefusesOnProjectedTOTAL_NotPerDocument(t *testing.T) {
	const newTitleLen = 255 // the title bound; the worst title that survives it
	const linkers = 3

	// Size each linker so that linkers-1 of them fit under the cap and all of
	// them do not. Derived from the cap rather than hardcoded, so the test
	// keeps discriminating if the cap moves.
	perDocTarget := (MaxRenameCascadeProjectedBytes / linkers) + (MaxRenameCascadeProjectedBytes / (linkers * 4))
	occurrences := perDocTarget / (5 + (newTitleLen - 1))
	body, perDocProjected := linkerBody(occurrences, newTitleLen)

	// Preconditions — these are what make the test discriminate. If either
	// fails the test is no longer testing what its name says.
	if perDocProjected >= MaxRenameCascadeProjectedBytes {
		t.Fatalf("precondition: per-document projection %d must be UNDER the cap %d, "+
			"otherwise a per-document guard would also pass this test",
			perDocProjected, MaxRenameCascadeProjectedBytes)
	}
	if total := perDocProjected * linkers; total <= MaxRenameCascadeProjectedBytes {
		t.Fatalf("precondition: total projection %d must EXCEED the cap %d",
			total, MaxRenameCascadeProjectedBytes)
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "CascadeTotalBound")
	target := createTestDoc(t, s, ws.ID, "A", "the document being renamed")

	linkerIDs := make([]string, 0, linkers)
	for i := 0; i < linkers; i++ {
		d := createTestDoc(t, s, ws.ID, "Linker"+string(rune('a'+i)), body)
		linkerIDs = append(linkerIDs, d.ID)
	}

	newTitle := strings.Repeat("T", newTitleLen)
	_, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle})

	// 1. Refused, and refused as THIS error. Without the guard this returns
	//    nil, having allocated the sum.
	if !errors.Is(err, ErrRenameCascadeTooLarge) {
		t.Fatalf("rename: got %v, want ErrRenameCascadeTooLarge", err)
	}

	// 2. Refused with the projection in the message. The only actionable
	//    information for a caller is what was projected against what is
	//    allowed; an error that says "too large" and nothing else sends them
	//    guessing.
	if msg := err.Error(); !strings.Contains(msg, "maximum") || !strings.Contains(msg, "bytes") {
		t.Errorf("error message lacks the projection: %q", msg)
	}

	// 3. The rename ROLLED BACK. A guard that refused after writing some of
	//    the linkers would pass assertion 1 and leave the workspace with a
	//    half-cascaded rename — the exact inconsistency the cascade's
	//    all-or-nothing transaction exists to prevent.
	after, err := s.GetDocument(target.ID)
	if err != nil {
		t.Fatalf("read back renamed document: %v", err)
	}
	if after.Title != "A" {
		t.Errorf("title = %q after a refused rename, want it unchanged at %q", after.Title, "A")
	}
	for i, id := range linkerIDs {
		got, err := s.GetDocument(id)
		if err != nil {
			t.Fatalf("read back linker %d: %v", i, err)
		}
		if got.Content != body {
			t.Errorf("linker %d content changed by a refused rename (len %d, want %d)",
				i, len(got.Content), len(body))
		}
	}
}

// TestRenameCascade_AllowsAnOrdinaryRename is the control leg. Without it, a
// guard that refused every rename — or a cap accidentally set near zero —
// passes the test above while breaking the feature outright.
//
// The shape is deliberately the same as the refusal case with one fewer
// linker, so the only difference between green and red is the total.
func TestRenameCascade_AllowsAnOrdinaryRename(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "CascadeOrdinary")
	target := createTestDoc(t, s, ws.ID, "A", "the document being renamed")
	linker := createTestDoc(t, s, ws.ID, "Linker", "before [[A]] middle [[A]] after")

	newTitle := "A Perfectly Ordinary Renamed Document"
	if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("ordinary rename refused: %v", err)
	}

	got, err := s.GetDocument(linker.ID)
	if err != nil {
		t.Fatalf("read back linker: %v", err)
	}
	want := "before [[" + newTitle + "]] middle [[" + newTitle + "]] after"
	if got.Content != want {
		t.Errorf("cascade did not rewrite the links:\n got: %q\nwant: %q", got.Content, want)
	}
}

// TestRenameCascade_RefusesTheSingleDocumentAttack covers the k=1 case
// directly: one linking document holding the largest body a 2 MiB request can
// carry still projects 108,632,370 bytes once the title bound is in place
// (measured), which is 6.5x the cap. The attack is refused at every k, not
// only at a threshold count of documents.
//
// Kept separate from the total-versus-per-document test because it is the one
// case a per-document guard WOULD catch — asserting both makes it explicit
// that the total guard is a superset, not a replacement of unclear scope.
func TestRenameCascade_RefusesTheSingleDocumentAttack(t *testing.T) {
	const newTitleLen = 255
	occurrences := (MaxRenameCascadeProjectedBytes / (5 + (newTitleLen - 1))) * 2 // 2x the cap
	body, projected := linkerBody(occurrences, newTitleLen)
	if projected <= MaxRenameCascadeProjectedBytes {
		t.Fatalf("precondition: single-document projection %d must exceed the cap %d", projected, MaxRenameCascadeProjectedBytes)
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "CascadeSingleDoc")
	target := createTestDoc(t, s, ws.ID, "A", "the document being renamed")
	createTestDoc(t, s, ws.ID, "Linker", body)

	newTitle := strings.Repeat("T", newTitleLen)
	_, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle})
	if !errors.Is(err, ErrRenameCascadeTooLarge) {
		t.Fatalf("rename: got %v, want ErrRenameCascadeTooLarge", err)
	}
}
