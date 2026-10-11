package acceptance

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		found        bool
		criteria     []Criterion
		placeholders []string
		raw          string
	}{
		{
			name:  "no heading",
			body:  "## Goals\n\n- AC-1: not under the heading\n",
			found: false,
		},
		{
			name:  "real criteria, section ends at the next same-level heading",
			body:  "## Context\n\nx\n\n## Acceptance criteria\n\n- AC-1: the copy is refused\n- AC-2: the key is released\n\n## Open questions\n\n- AC-3: not a criterion\n",
			found: true,
			criteria: []Criterion{
				{ID: "AC-1", Text: "the copy is refused"},
				{ID: "AC-2", Text: "the key is released"},
			},
			placeholders: []string{},
			raw:          "- AC-1: the copy is refused\n- AC-2: the key is released",
		},
		{
			name:  "a deeper heading stays inside the section",
			body:  "## Acceptance criteria\n\n### Server\n\n- AC-1: a\n\n# Next\n- AC-2: b\n",
			found: true,
			criteria: []Criterion{
				{ID: "AC-1", Text: "a"},
			},
			placeholders: []string{},
			raw:          "### Server\n\n- AC-1: a",
		},
		{
			name:  "skeleton lines are placeholders: angle brackets, bare ellipsis, empty",
			body:  "## Acceptance criteria\n\n- AC-1: <a statement>\n- AC-2: ...\n- AC-3: …\n- AC-4:\n- AC-5: real <b>bold</b> text\n",
			found: true,
			criteria: []Criterion{
				{ID: "AC-5", Text: "real <b>bold</b> text"},
			},
			placeholders: []string{"AC-1", "AC-2", "AC-3", "AC-4"},
		},
		{
			name:  "indented continuation joins; prose and other bullets end it",
			body:  "## Acceptance criteria\n- AC-1: first line\n  second line\n- not a criterion\n  stray indent\n* AC-2: star bullet\n",
			found: true,
			criteria: []Criterion{
				{ID: "AC-1", Text: "first line second line"},
				{ID: "AC-2", Text: "star bullet"},
			},
			placeholders: []string{},
		},
		{
			name:  "headings and AC lines inside a code fence are ignored",
			body:  "```\n## Acceptance criteria\n- AC-9: fenced\n```\n## Acceptance Criteria ##\n- AC-1: real\n~~~\n- AC-2: fenced too\n## Not a heading here\n~~~\n- AC-3: after the fence\n",
			found: true,
			criteria: []Criterion{
				{ID: "AC-1", Text: "real"},
				{ID: "AC-3", Text: "after the fence"},
			},
			placeholders: []string{},
		},
		{
			name:         "heading with nothing under it",
			body:         "## Acceptance criteria\n\n## Next\n",
			found:        true,
			criteria:     []Criterion{},
			placeholders: []string{},
			raw:          "",
		},
		{
			name:  "CRLF bodies",
			body:  "## Acceptance criteria\r\n- AC-1: windows\r\n",
			found: true,
			criteria: []Criterion{
				{ID: "AC-1", Text: "windows"},
			},
			placeholders: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.body)
			if got.Found != tc.found {
				t.Fatalf("Found = %v, want %v", got.Found, tc.found)
			}
			if !tc.found {
				if got.Criteria != nil || got.Placeholders != nil || got.Raw != "" {
					t.Fatalf("not found, but got %+v", got)
				}
				return
			}
			if !reflect.DeepEqual(got.Criteria, tc.criteria) {
				t.Errorf("Criteria = %#v, want %#v", got.Criteria, tc.criteria)
			}
			if !reflect.DeepEqual(got.Placeholders, tc.placeholders) {
				t.Errorf("Placeholders = %#v, want %#v", got.Placeholders, tc.placeholders)
			}
			if tc.raw != "" || tc.name == "heading with nothing under it" {
				if got.Raw != tc.raw {
					t.Errorf("Raw = %q, want %q", got.Raw, tc.raw)
				}
			}
		})
	}
}
