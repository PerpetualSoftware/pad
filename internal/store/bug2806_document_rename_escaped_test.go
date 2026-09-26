package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/links"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2806. A legacy document title containing `|`, `\` or `]` (refused for
// new titles since BUG-2798) is linked in the grammar's ESCAPED form, and the
// document rename cascade matched only the raw literal, which for such a
// title is not a link to it at all: `[[A|B]]` is a link to `A` displaying
// `B`. So renaming the document left every real link stale and rewrote a link
// to a different target. testStore runs on Postgres when
// PAD_TEST_POSTGRES_URL is set, which is the dialect whose LIKE treats `\`
// as an escape.
func TestDocumentRenameFollowsEscapedLinks(t *testing.T) {
	for _, tc := range []struct {
		legacy, escapedLink, otherLink string
	}{
		{`A|B`, `[[A\|B]]`, `[[A|B]]`},
		{`A\B`, `[[A\\B]]`, `[[A\B]]`},
		{`A]B`, `[[A\]B]]`, `[[A]B]]`},
	} {
		t.Run(tc.legacy, func(t *testing.T) {
			s := testStore(t)
			ws := createTestWorkspace(t, s, "W")
			target, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "placeholder"})
			if err != nil {
				t.Fatal(err)
			}
			// A title from before BUG-2798's refusal: written directly, as a
			// legacy row would have been.
			if _, err := s.DB().Exec(s.q(`UPDATE documents SET title = ? WHERE id = ?`), tc.legacy, target.ID); err != nil {
				t.Fatal(err)
			}
			body := "real " + tc.escapedLink + " / other " + tc.otherLink + " / plain [[Unrelated]]"
			linker, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Linker", Content: body})
			if err != nil {
				t.Fatal(err)
			}

			// A linker holding ONLY the escaped link: the scan must find it by
			// that form, not by the raw literal the first linker also carries.
			onlyEscaped, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Only escaped", Content: "just " + tc.escapedLink})
			if err != nil {
				t.Fatal(err)
			}

			newTitle := "Fresh"
			if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle}); err != nil {
				t.Fatalf("rename: %v", err)
			}
			got, err := s.GetDocument(linker.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "real [[Fresh]] / other " + tc.otherLink + " / plain [[Unrelated]]"
			if got.Content != want {
				t.Fatalf("linker after rename:\n got %q\nwant %q", got.Content, want)
			}
			only, err := s.GetDocument(onlyEscaped.ID)
			if err != nil {
				t.Fatal(err)
			}
			if only.Content != "just [[Fresh]]" {
				t.Fatalf("a linker holding only the escaped link was not rewritten: %q", only.Content)
			}
		})
	}
}

// The cascade's memory bound projects the rewritten length before building
// it. It must project what ReplaceTitle actually writes, which since BUG-2806
// is the ESCAPED title: a projection from raw lengths is wrong by one byte per
// escaped character per link, in whichever direction the titles differ.
func TestCascadeRetainedBytesProjectsTheEscapedRewrite(t *testing.T) {
	for _, tc := range []struct{ old, new string }{
		{`A|B`, "Fresh"}, {`A\B`, "Fresh"}, {`A]B`, "x"}, {"Plain", "Longer title"}, {"Fresh", `A|B|C`},
	} {
		read := "one [[" + links.EscapeWikiTitle(tc.old) + "]] two [[" + links.EscapeWikiTitle(tc.old) + "]] three"
		occ := int64(strings.Count(read, "[["+links.EscapeWikiTitle(tc.old)+"]]"))
		got := cascadeRetainedBytes(read, occ, tc.old, tc.new)
		want := int64(len(read) + len(links.ReplaceTitle(read, tc.old, tc.new)))
		if got != want {
			t.Errorf("%q -> %q: projected %d, ReplaceTitle holds %d", tc.old, tc.new, got, want)
		}
	}
}
