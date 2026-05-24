package links

import (
	"strings"
	"testing"
)

// TestExtractWikiLinks_RefForm covers the Phase 1 happy path: ref-form
// links outside any code region are extracted with correct kind, ref,
// display, and position.
func TestExtractWikiLinks_RefForm(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []WikiLinkRef
	}{
		{
			name:    "single bare ref",
			content: "See [[TASK-5]] for context.",
			want: []WikiLinkRef{
				{Kind: WikiLinkKindRef, Ref: "TASK-5", Position: 4},
			},
		},
		{
			name:    "ref with display alias",
			content: "Per [[TASK-5|the auth fix]] we...",
			want: []WikiLinkRef{
				{Kind: WikiLinkKindRef, Ref: "TASK-5", Display: "the auth fix", Position: 4},
			},
		},
		{
			name:    "multiple refs same content",
			content: "[[TASK-1]] depends on [[BUG-2]] and [[IDEA-3]].",
			want: []WikiLinkRef{
				{Kind: WikiLinkKindRef, Ref: "TASK-1", Position: 0},
				{Kind: WikiLinkKindRef, Ref: "BUG-2", Position: 22},
				{Kind: WikiLinkKindRef, Ref: "IDEA-3", Position: 36},
			},
		},
		{
			name:    "ref repeated in same body",
			content: "[[FOO-1]] and again [[FOO-1]].",
			want: []WikiLinkRef{
				{Kind: WikiLinkKindRef, Ref: "FOO-1", Position: 0},
				{Kind: WikiLinkKindRef, Ref: "FOO-1", Position: 20},
			},
		},
		{
			name:    "long collection prefix",
			content: "Linked to [[PLAYBOOK-12345]].",
			want: []WikiLinkRef{
				{Kind: WikiLinkKindRef, Ref: "PLAYBOOK-12345", Position: 10},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractWikiLinks(tc.content)
			assertLinks(t, got, tc.want)
		})
	}
}

// TestExtractWikiLinks_NonRefFormsHidden verifies that Phase 1's gate
// suppresses title and workspace_ref kinds from the returned slice —
// even though parseBody recognizes them. Once Phase 2 lands and the
// gate is removed, these inputs must start emitting WikiLinkRefs of
// the corresponding kinds; this test will need updating then.
func TestExtractWikiLinks_NonRefFormsHidden(t *testing.T) {
	cases := []string{
		"Click [[Some Title]] here.",           // legacy title
		"Look at [[docs/Setup]] for help.",     // collection-qualified
		"Cross [[other-ws::TASK-9]] over.",     // workspace_ref
		"Cross [[other-ws::TASK-9|over]] too.", // workspace_ref + display
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			got := ExtractWikiLinks(c)
			if len(got) != 0 {
				t.Errorf("expected 0 links (Phase 1 hides non-ref forms), got %d: %+v", len(got), got)
			}
		})
	}
}

// TestExtractWikiLinks_CodeBlocksExcluded asserts the headline behavior
// decision from PLAN-1593: [[REF]] inside fenced or inline code is NOT
// a real link and must be skipped.
func TestExtractWikiLinks_CodeBlocksExcluded(t *testing.T) {
	t.Run("fenced block excludes refs inside", func(t *testing.T) {
		content := "Real: [[OUTSIDE-1]]\n" +
			"```\n" +
			"Example: [[INSIDE-1]] and [[INSIDE-2]]\n" +
			"```\n" +
			"After: [[OUTSIDE-2]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"OUTSIDE-1", "OUTSIDE-2"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("fenced block with language tag", func(t *testing.T) {
		content := "Before: [[A-1]]\n" +
			"```bash\n" +
			"echo [[B-1]]\n" +
			"```\n" +
			"After: [[C-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"A-1", "C-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("inline code excludes ref inside", func(t *testing.T) {
		content := "The `[[FAKE-1]]` syntax has shipped; see [[REAL-2]] for examples."
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"REAL-2"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("unclosed fence runs to EOF", func(t *testing.T) {
		// A draft with an opened-but-never-closed fence should treat
		// everything after the fence as code (matches markdown
		// rendering of the same draft).
		content := "Before: [[A-1]]\n" +
			"```\n" +
			"After: [[B-1]] // inside dangling fence — must be skipped"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"A-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("inline code spans single newline (CommonMark §6.1)", func(t *testing.T) {
		// Per CommonMark, an inline-code span can cross a single
		// newline. The renderer treats `pre\n[[INSIDE-1]]\npost`
		// as a single code span — the embedded link must NOT be
		// indexed. Codex round-9 P1.
		content := "Outside [[A-1]]\n" +
			"start `pre\n[[INSIDE-1]]\npost` end\n" +
			"Outside [[B-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"A-1", "B-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("inline code breaks at blank line (paragraph boundary)", func(t *testing.T) {
		// A blank line terminates the paragraph, so a still-open
		// inline-code span must end at the blank-line break.
		// Refs in subsequent paragraphs are NOT part of the span
		// and must be indexed.
		content := "Outside [[A-1]]\n" +
			"open `unclosed-span\n" +
			"\n" +
			"new paragraph [[REAL-1]] more text\n" +
			"Outside [[B-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"A-1", "REAL-1", "B-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("inline code breaks at whitespace-only blank line", func(t *testing.T) {
		// CommonMark treats a line containing only spaces/tabs as
		// blank. Span must still terminate there.
		content := "open `unclosed-span\n" +
			"  \t  \n" +
			"after-blank [[REAL-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"REAL-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("inline code closer matches opener length", func(t *testing.T) {
		// CommonMark §6.1: a span opened with N backticks closes
		// only on a run of EXACTLY N backticks. Stray single
		// backticks inside a ``...`` span are code text, not
		// closers. Without matching-run semantics, the extractor
		// would close on the first single backtick and leak the
		// inner [[X]]. Codex round-7 finding #2.
		content := "Before [[A-1]] ``code with ` inside and [[INSIDE-1]]`` after [[B-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"A-1", "B-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("single-backtick span unaffected by adjacent multi-backtick run", func(t *testing.T) {
		// `code` is a normal one-backtick span that closes on the
		// next single backtick. A double-backtick run is NOT a
		// valid closer for a single-backtick opener.
		content := "Try `code [[INSIDE-1]] ``not-closer` then [[REAL-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"REAL-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("multi-backtick inline code excludes ref", func(t *testing.T) {
		// CommonMark inline-code spans support multi-backtick
		// delimiters (`code` and ``code`` are both spans). Our
		// permissive matcher treats any backtick run as an opener
		// and closes on the next backtick run — which covers this
		// case correctly: ``see [[X]]`` becomes a single [0, end)
		// range, the embedded link is excluded.
		content := "``see [[INSIDE-1]]`` and [[REAL-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"REAL-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("tilde fence excludes refs inside", func(t *testing.T) {
		// CommonMark / marked accept ~~~ fences alongside ```.
		// Refs inside a tilde fence render as code in the UI and
		// must NOT be indexed. Codex round-6 finding #1.
		content := "Real: [[OUTSIDE-1]]\n" +
			"~~~\n" +
			"Example: [[INSIDE-1]]\n" +
			"~~~\n" +
			"After: [[OUTSIDE-2]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"OUTSIDE-1", "OUTSIDE-2"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("tilde fence with language tag", func(t *testing.T) {
		content := "Before [[A-1]]\n" +
			"~~~python\n" +
			"# echo [[B-1]]\n" +
			"~~~\n" +
			"After [[C-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"A-1", "C-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("mixed fence types don't pair", func(t *testing.T) {
		// A backtick opener must NOT be closed by a tilde line and
		// vice versa. If the wrong char appears, the fence stays
		// open until the right char (or EOF) is found.
		content := "Before [[A-1]]\n" +
			"```\n" +
			"~~~ // not a closer for the ``` opener\n" +
			"[[INSIDE-1]]\n" +
			"```\n" +
			"After [[B-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"A-1", "B-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("closer-line strictness — backticks plus other text is not a closer", func(t *testing.T) {
		// CommonMark §4.5: a closing fence line must contain only
		// the fence + optional trailing spaces. A line like
		// `\`\`\`not-closed` inside an open fence does NOT
		// terminate the block — later refs in the same fence stay
		// excluded. Codex round-6 finding #2.
		content := "Real: [[OUTSIDE-1]]\n" +
			"```\n" +
			"```not-closed\n" +
			"[[INSIDE-1]] still inside the fence\n" +
			"```\n" +
			"After: [[OUTSIDE-2]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"OUTSIDE-1", "OUTSIDE-2"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("closer-line strictness — trailing spaces OK", func(t *testing.T) {
		// A real closer may have trailing whitespace.
		content := "Before [[A-1]]\n" +
			"```\n" +
			"[[INSIDE-1]]\n" +
			"```   \n" +
			"After [[B-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"A-1", "B-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("indented fenced block (CommonMark 0-3 spaces)", func(t *testing.T) {
		// CommonMark allows fenced code blocks indented 0-3 spaces;
		// marked() (the renderer's markdown parser) implements this.
		// Our extractor must mirror or we false-positive on every
		// indented code example users write.
		content := "Before [[A-1]]\n" +
			"   ```\n" +
			"   echo [[INSIDE-1]]\n" +
			"   ```\n" +
			"After [[B-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"A-1", "B-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("inline code adjacent to real link", func(t *testing.T) {
		// `code-with-link` then [[REAL-1]] — closer of inline span
		// must not accidentally include the real link.
		content := "Try `[[A-1]]` and then [[REAL-1]]."
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"REAL-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})

	t.Run("fenced block at start of content (no leading newline)", func(t *testing.T) {
		content := "```\n[[INSIDE-1]]\n```\n[[OUTSIDE-1]]"
		got := ExtractWikiLinks(content)
		refs := refStrings(got)
		want := []string{"OUTSIDE-1"}
		if !equalStringSlices(refs, want) {
			t.Errorf("got refs %v, want %v", refs, want)
		}
	})
}

// TestExtractWikiLinks_Edge covers malformed/weird inputs the renderer
// gracefully falls through on. Our extractor must do the same.
func TestExtractWikiLinks_Edge(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty content", ""},
		{"no links", "Plain text with no wiki links at all."},
		{"empty brackets", "Here is [[]] which shouldn't match."},
		{"single bracket", "[A-1] should not match."},
		{"nested brackets", "[[[A-1]]] shouldn't either."},
		{"number-led not a ref", "[[5-task]] is not a ref shape."},
		{"missing number", "[[TASK]] needs a number."},
		{"bracket with newline body", "[[A-1\nB-2]] is malformed."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractWikiLinks(c.content)
			for _, g := range got {
				// Anything emitted must at least be a valid ref-form.
				if g.Kind != WikiLinkKindRef {
					t.Errorf("emitted non-ref kind in Phase 1: %+v", g)
				}
				if g.Ref == "" {
					t.Errorf("emitted ref-kind with empty Ref: %+v", g)
				}
			}
		})
	}
}

// TestExtractWikiLinks_PositionIsByteOffset asserts that Position
// points at the opening `[[` in the ORIGINAL content — the handler
// uses this to extract a ~80-char snippet centered on the match.
func TestExtractWikiLinks_PositionIsByteOffset(t *testing.T) {
	content := "Some prose. [[TASK-99]] more prose."
	got := ExtractWikiLinks(content)
	if len(got) != 1 {
		t.Fatalf("want 1 link, got %d", len(got))
	}
	pos := got[0].Position
	if got := content[pos : pos+2]; got != "[[" {
		t.Errorf("Position should point at `[[`, got %q", got)
	}
}

// TestExtractWikiLinks_RefVsTitleFallback exercises the parseBody
// decision tree. After Codex round-1 P2, the refPattern is
// case-insensitive — `[[Task-5]]` and `[[task-5]]` now parse as the
// ref kind (and normalize to "TASK-5" for storage), matching the
// renderer's behavior. Inputs that don't look like a ref at all
// still fall through to title.
func TestExtractWikiLinks_RefVsTitleFallback(t *testing.T) {
	t.Run("uppercase ref-shaped body parses as ref", func(t *testing.T) {
		got := ExtractWikiLinks("[[TASK-5]]")
		if len(got) != 1 || got[0].Kind != WikiLinkKindRef {
			t.Errorf("expected single ref kind, got %+v", got)
		}
		if got[0].Ref != "TASK-5" {
			t.Errorf("Ref: got %q, want TASK-5", got[0].Ref)
		}
	})
	t.Run("mixed-case body parses as ref, normalized to uppercase", func(t *testing.T) {
		got := ExtractWikiLinks("[[Task-5]]")
		if len(got) != 1 {
			t.Fatalf("expected 1 ref-kind result (case-insensitive match), got %d: %+v", len(got), got)
		}
		if got[0].Kind != WikiLinkKindRef {
			t.Errorf("Kind: got %q, want ref", got[0].Kind)
		}
		if got[0].Ref != "TASK-5" {
			t.Errorf("Ref should be canonicalized to uppercase: got %q want TASK-5", got[0].Ref)
		}
	})
	t.Run("lowercase body parses as ref, normalized to uppercase", func(t *testing.T) {
		got := ExtractWikiLinks("[[task-5]]")
		if len(got) != 1 {
			t.Fatalf("expected 1 ref-kind result, got %d", len(got))
		}
		if got[0].Ref != "TASK-5" {
			t.Errorf("Ref: got %q want TASK-5", got[0].Ref)
		}
	})
	t.Run("non-ref body falls to title and is hidden", func(t *testing.T) {
		// "5-Task" (number-led) doesn't match REF_PATTERN even
		// with the relaxed case rule; parseBody returns a
		// title-kind ref; Phase 1 gates it out.
		got := ExtractWikiLinks("[[5-Task]]")
		if len(got) != 0 {
			t.Errorf("expected 0 (title-kind hidden), got %+v", got)
		}
	})
}

// TestExtractWikiLinks_EscapedBodyChars regresses Codex rounds
// 4/7/10 P2: the editor's wikiLinksToMarkdown can produce bodies
// containing `\]`, `\|`, `\\` escapes (markdown.ts:461). The
// extractor must parse those — both the regex and the body parser —
// so the resulting link is indexed with the unescaped display text.
func TestExtractWikiLinks_EscapedBodyChars(t *testing.T) {
	t.Run("escaped closing bracket in display", func(t *testing.T) {
		// [[TASK-1|see \] bracket]] — display is "see ] bracket".
		got := ExtractWikiLinks(`[[TASK-1|see \] bracket]]`)
		if len(got) != 1 {
			t.Fatalf("expected 1 ref, got %d: %+v", len(got), got)
		}
		if got[0].Ref != "TASK-1" {
			t.Errorf("Ref: got %q want TASK-1", got[0].Ref)
		}
		if got[0].Display != "see ] bracket" {
			t.Errorf("Display: got %q want %q", got[0].Display, "see ] bracket")
		}
	})

	t.Run("escaped pipe in display", func(t *testing.T) {
		// [[TASK-2|A \| B]] — display is "A | B"; the unescaped
		// pipe doesn't split the body.
		got := ExtractWikiLinks(`[[TASK-2|A \| B]]`)
		if len(got) != 1 {
			t.Fatalf("expected 1 ref, got %d", len(got))
		}
		if got[0].Display != "A | B" {
			t.Errorf("Display: got %q want %q", got[0].Display, "A | B")
		}
	})

	t.Run("escaped backslash in display", func(t *testing.T) {
		// [[TASK-3|a \\ b]] — display is "a \ b".
		got := ExtractWikiLinks(`[[TASK-3|a \\ b]]`)
		if len(got) != 1 {
			t.Fatalf("expected 1 ref, got %d", len(got))
		}
		if got[0].Display != `a \ b` {
			t.Errorf("Display: got %q want %q", got[0].Display, `a \ b`)
		}
	})

	t.Run("non-escape backslash passes through", func(t *testing.T) {
		// `\n` (or any `\X` where X isn't ]|\) is left alone.
		got := ExtractWikiLinks(`[[TASK-4|a \n b]]`)
		if len(got) != 1 {
			t.Fatalf("expected 1 ref, got %d", len(got))
		}
		if got[0].Display != `a \n b` {
			t.Errorf("Display: got %q want %q", got[0].Display, `a \n b`)
		}
	})

	t.Run("explicit empty display override is distinguished from no pipe", func(t *testing.T) {
		// [[X|]] is distinct from [[X]] in the editor: the former
		// has displayOverride="" (preserved by JS ?? coalescing),
		// the latter has no override and falls back to title. We
		// preserve that distinction via HasDisplay. Codex round-12 P3.
		withPipe := ExtractWikiLinks(`[[TASK-7|]]`)
		if len(withPipe) != 1 {
			t.Fatalf("expected 1 ref, got %d", len(withPipe))
		}
		if !withPipe[0].HasDisplay {
			t.Errorf("[[TASK-7|]] should have HasDisplay=true")
		}
		if withPipe[0].Display != "" {
			t.Errorf("[[TASK-7|]] Display should be \"\", got %q", withPipe[0].Display)
		}

		noPipe := ExtractWikiLinks(`[[TASK-7]]`)
		if len(noPipe) != 1 {
			t.Fatalf("expected 1 ref, got %d", len(noPipe))
		}
		if noPipe[0].HasDisplay {
			t.Errorf("[[TASK-7]] should have HasDisplay=false")
		}
		if noPipe[0].Display != "" {
			t.Errorf("[[TASK-7]] Display should be \"\", got %q", noPipe[0].Display)
		}
	})

	t.Run("display text preserved verbatim (no TrimSpace)", func(t *testing.T) {
		// The renderer stores display text verbatim — leading and
		// trailing whitespace are part of the display. Trimming
		// in the extractor would silently differ from client
		// behavior. Codex round-11 P3.
		got := ExtractWikiLinks(`[[TASK-9|  padded display  ]]`)
		if len(got) != 1 {
			t.Fatalf("expected 1 ref, got %d", len(got))
		}
		if got[0].Display != "  padded display  " {
			t.Errorf("Display should be verbatim, got %q", got[0].Display)
		}
	})

	t.Run("position still points at opening [[", func(t *testing.T) {
		// Escapes shouldn't shift Position — it's the byte offset
		// in the ORIGINAL content, not the unescaped form.
		content := "Prefix " + `[[TASK-5|see \] here]]` + " suffix"
		got := ExtractWikiLinks(content)
		if len(got) != 1 {
			t.Fatalf("expected 1 ref, got %d", len(got))
		}
		if content[got[0].Position:got[0].Position+2] != "[[" {
			t.Errorf("Position should point at `[[`, got %q",
				content[got[0].Position:got[0].Position+2])
		}
	})
}

// TestSplitOnUnescapedPipe / TestUnescapeWikiBody — direct unit
// tests for the helpers. Round-trip safety against the editor's
// serializer (escapeWikiBody/unescapeWikiBody in markdown.ts:652-658)
// is the property we care about.
func TestSplitOnUnescapedPipe(t *testing.T) {
	cases := []struct {
		in         string
		wantKey    string
		wantSuffix string
		wantFound  bool
	}{
		{"ref-only", "ref-only", "", false},
		{"key|display", "key", "display", true},
		{`key\|with-pipe`, `key\|with-pipe`, "", false},
		{`first\|second|third`, `first\|second`, "third", true},
		{`\\|trailing`, `\\`, "trailing", true}, // \\ is escaped backslash, then |
	}
	for _, c := range cases {
		k, s, ok := splitOnUnescapedPipe(c.in)
		if k != c.wantKey || s != c.wantSuffix || ok != c.wantFound {
			t.Errorf("splitOnUnescapedPipe(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, k, s, ok, c.wantKey, c.wantSuffix, c.wantFound)
		}
	}
}

func TestUnescapeWikiBody(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{`\]`, "]"},
		{`\|`, "|"},
		{`\\`, `\`},
		{`a \] b \| c \\ d`, `a ] b | c \ d`},
		{`\n stays literal`, `\n stays literal`},
		{"", ""},
	}
	for _, c := range cases {
		got := unescapeWikiBody(c.in)
		if got != c.want {
			t.Errorf("unescapeWikiBody(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCanonicalizeRef is the unit-level check on the helper. The
// integration coverage lives in TestWikiLinks_MixedCaseRefIndexed
// (store) — but the helper's edge cases (no-hyphen, all-uppercase
// already, multi-hyphen prefix) are easier to assert directly.
func TestCanonicalizeRef(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"TASK-5", "TASK-5"},
		{"task-5", "TASK-5"},
		{"Task-5", "TASK-5"},
		{"PLAYBOOK-12345", "PLAYBOOK-12345"},
		{"playbook-12345", "PLAYBOOK-12345"},
	}
	for _, tc := range cases {
		got := canonicalizeRef(tc.in)
		if got != tc.want {
			t.Errorf("canonicalizeRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// assertLinks compares two WikiLinkRef slices for the fields Phase 1
// cares about. Doesn't enforce equality on fields not yet emitted
// (Title, WorkspaceSlug) so adding test cases for those in Phase 2
// won't require rewriting these comparisons.
func assertLinks(t *testing.T, got, want []WikiLinkRef) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d links, want %d: got=%+v want=%+v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i].Kind != want[i].Kind {
			t.Errorf("[%d] Kind: got %q, want %q", i, got[i].Kind, want[i].Kind)
		}
		if got[i].Ref != want[i].Ref {
			t.Errorf("[%d] Ref: got %q, want %q", i, got[i].Ref, want[i].Ref)
		}
		if got[i].Display != want[i].Display {
			t.Errorf("[%d] Display: got %q, want %q", i, got[i].Display, want[i].Display)
		}
		if got[i].Position != want[i].Position {
			t.Errorf("[%d] Position: got %d, want %d", i, got[i].Position, want[i].Position)
		}
	}
}

func refStrings(links []WikiLinkRef) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.Ref
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Sanity check: position is BYTE offset, not rune offset. A leading
// multi-byte rune shifts the [[ to a position > its rune index.
func TestExtractWikiLinks_PositionByteVsRune(t *testing.T) {
	// "héllo " has é = 2 bytes (UTF-8). The [[ that follows should
	// be at byte offset 7 ("h"=1 + "é"=2 + "llo "=4 = 7), even
	// though its rune offset is only 6.
	content := "héllo [[TASK-1]]"
	got := ExtractWikiLinks(content)
	if len(got) != 1 {
		t.Fatalf("expected 1 link, got %d", len(got))
	}
	if got[0].Position != strings.Index(content, "[[") {
		t.Errorf("Position should be byte offset matching strings.Index, got %d want %d",
			got[0].Position, strings.Index(content, "[["))
	}
}
