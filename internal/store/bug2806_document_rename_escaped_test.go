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
		legacy string
		// links are every encoding of a link to legacy: each must be rewritten.
		links []string
		// others are NOT links to legacy by the grammar and must be untouched.
		others []string
	}{
		// `[[A|B]]` is a link to `A` displaying `B`.
		{`A|B`, []string{`[[A\|B]]`}, []string{`[[A|B]]`}},
		// A `\` before an ordinary character is literal, so the raw and the
		// doubled form both decode to `A\B`.
		{`A\B`, []string{`[[A\\B]]`, `[[A\B]]`}, []string{`[[AB]]`, `[[A\B|alias]]`}},
		// `[[A]B]]` is a link to `A` followed by the text `B]]`.
		{`A]B`, []string{`[[A\]B]]`}, []string{`[[A]B]]`}},
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
			body, want := "", ""
			for _, l := range tc.links {
				body += "link " + l + " / "
				want += "link [[Fresh]] / "
			}
			for _, o := range tc.others {
				body += "other " + o + " / "
				want += "other " + o + " / "
			}
			linker, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Linker", Content: body})
			if err != nil {
				t.Fatal(err)
			}
			// One linker per encoding, holding ONLY that encoding: the scan
			// must find each by its own form, not by a sibling in the same body.
			var singles []*models.Document
			for _, l := range tc.links {
				d, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Only " + l, Content: "just " + l})
				if err != nil {
					t.Fatal(err)
				}
				singles = append(singles, d)
			}

			newTitle := "Fresh"
			if _, err := s.UpdateDocument(target.ID, models.DocumentUpdate{Title: &newTitle}); err != nil {
				t.Fatalf("rename: %v", err)
			}
			got, err := s.GetDocument(linker.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Content != want {
				t.Fatalf("linker after rename:\n got %q\nwant %q", got.Content, want)
			}
			for i, d := range singles {
				got, err := s.GetDocument(d.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Content != "just [[Fresh]]" {
					t.Errorf("a linker holding only %s was not rewritten: %q", tc.links[i], got.Content)
				}
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
		// Two encodings of the same link where the title has one: the
		// projection must price each by its own length.
		read := "one [[" + links.EscapeWikiTitle(tc.old) + "]] two [[" + strings.ReplaceAll(links.EscapeWikiTitle(tc.old), `\\`, `\`) + "]] three"
		got := cascadeRetainedBytes(read, tc.old, tc.new)
		want := int64(len(read) + len(links.ReplaceTitle(read, tc.old, tc.new)))
		if got != want {
			t.Errorf("%q -> %q: projected %d, ReplaceTitle holds %d", tc.old, tc.new, got, want)
		}
	}
}
