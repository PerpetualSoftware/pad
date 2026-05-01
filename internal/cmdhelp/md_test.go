package cmdhelp

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fixedClock returns a deterministic time for snapshot stability.
func fixedClock() time.Time {
	return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
}

// renderMD is a small test helper that builds + renders the synthetic
// tree with a fixed clock and returns the markdown output as a string.
func renderMD(t *testing.T, opts Options) string {
	t.Helper()
	root := buildSyntheticTree()
	if opts.Binary == "" {
		opts.Binary = "padtest"
	}
	if opts.Now == nil {
		opts.Now = fixedClock
	}
	if opts.MaxDepth == 0 {
		opts.MaxDepth = -1
	}
	var buf bytes.Buffer
	if err := EmitMarkdown(root, root, opts, &buf); err != nil {
		t.Fatalf("EmitMarkdown: %v", err)
	}
	return buf.String()
}

func TestRenderMarkdown_Frontmatter(t *testing.T) {
	out := renderMD(t, Options{
		Binary:  "padtest",
		Version: "9.9.9",
	})
	// Frontmatter must be the first thing in the document.
	if !strings.HasPrefix(out, "---\n") {
		t.Fatalf("output must start with YAML frontmatter, got first line: %q", firstLine(out))
	}
	want := []string{
		`cmdhelp_version: "0.1"`,
		"binary: padtest",
		"version: 9.9.9",
		"generated: 2026-05-01T12:00:00Z",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("frontmatter missing %q\noutput head:\n%s", w, head(out, 10))
		}
	}
	// Closing frontmatter delimiter present.
	if !strings.Contains(out, "\n---\n\n# padtest") {
		t.Errorf("closing frontmatter delimiter missing or H1 not immediately after")
	}
}

func TestRenderMarkdown_FrontmatterTimestampOverridable(t *testing.T) {
	// Inject a different clock and verify the timestamp follows.
	custom := func() time.Time {
		return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	out := renderMD(t, Options{Now: custom})
	if !strings.Contains(out, "generated: 2030-01-01T00:00:00Z") {
		t.Errorf("expected injected timestamp in frontmatter; first 10 lines:\n%s", head(out, 10))
	}
}

func TestRenderMarkdown_SectionOrder(t *testing.T) {
	// Per spec §6, per-command section order is:
	//   ## `cmd`
	//   summary
	//   ### Synopsis
	//   ### Arguments
	//   ### Flags
	//   ### Stdin (when applicable)
	//   ### Examples
	//   ### Output
	//   ### See also
	//
	// Use the synthetic `item create` block — it has args, flags, and
	// examples, so we can assert the ordering of those sections.
	out := renderMD(t, Options{})
	itemBlock := extractCommandBlock(out, "item create")
	if itemBlock == "" {
		t.Fatalf("could not locate `item create` block in output:\n%s", head(out, 30))
	}

	wantOrder := []string{
		"## `padtest item create`",
		"### Synopsis",
		"### Arguments",
		"### Flags",
		"### Examples",
	}
	last := -1
	for _, marker := range wantOrder {
		idx := strings.Index(itemBlock, marker)
		if idx < 0 {
			t.Errorf("section %q missing from item-create block", marker)
			continue
		}
		if idx <= last {
			t.Errorf("section %q at index %d does not follow previous section (idx %d) — wrong order:\n%s",
				marker, idx, last, itemBlock)
		}
		last = idx
	}
}

func TestRenderMarkdown_SynopsisIncludesArgsAndFlagsHint(t *testing.T) {
	out := renderMD(t, Options{})
	itemBlock := extractCommandBlock(out, "item create")

	// Synopsis fence must contain the binary, command path, and arg tokens.
	wantIn := []string{
		"```\npadtest item create",
		"<collection>",
		"<title>",
		"[flags]", // because the command has flags
	}
	for _, w := range wantIn {
		if !strings.Contains(itemBlock, w) {
			t.Errorf("synopsis missing %q in:\n%s", w, itemBlock)
		}
	}
}

func TestRenderMarkdown_VariadicSynopsis(t *testing.T) {
	out := renderMD(t, Options{})
	bulkBlock := extractCommandBlock(out, "item bulk-update")
	if !strings.Contains(bulkBlock, "<ref>...") {
		t.Errorf("variadic <ref>... should appear in synopsis; block was:\n%s", bulkBlock)
	}
}

func TestRenderMarkdown_AlternationAsEnum(t *testing.T) {
	out := renderMD(t, Options{})
	completionBlock := extractCommandBlock(out, "item completion")
	// Alternation arg is rendered as enum: bash\|zsh\|fish\|powershell
	if !strings.Contains(completionBlock, "bash") || !strings.Contains(completionBlock, "powershell") {
		t.Errorf("expected enum values from alternation in arguments table; block:\n%s", completionBlock)
	}
}

func TestRenderMarkdown_GlobalFlagsTopLevelOnly(t *testing.T) {
	out := renderMD(t, Options{})
	// A "## Global flags" heading should appear.
	if !strings.Contains(out, "## Global flags") {
		t.Errorf("expected '## Global flags' heading at top level")
	}
	// The global flag --workspace should appear under the top-level
	// section, not in any per-command Flags block.
	for _, name := range []string{"item", "item create", "item update", "deep nest leaf"} {
		block := extractCommandBlock(out, name)
		if block == "" {
			continue
		}
		// It's fine for the per-command block to NOT have a Flags table
		// at all; we just want to make sure --workspace is not in it.
		if strings.Contains(block, "`--workspace`") {
			t.Errorf("global flag --workspace must not be duplicated in `%s` block", name)
		}
	}
}

func TestRenderMarkdown_ExamplesAsFencedBash(t *testing.T) {
	out := renderMD(t, Options{})
	itemBlock := extractCommandBlock(out, "item create")

	if !strings.Contains(itemBlock, "### Examples") {
		t.Fatalf("Examples section missing from item-create block")
	}
	// The synthetic tree's Example field has two non-comment lines.
	// Each should be rendered as a separate ```bash ... ``` block.
	count := strings.Count(itemBlock, "```bash\n")
	if count < 2 {
		t.Errorf("expected at least 2 fenced bash example blocks, got %d", count)
	}
	if !strings.Contains(itemBlock, "Fix OAuth") {
		t.Errorf("expected first example to mention Fix OAuth")
	}
	// Comment line from the synthetic Example field must NOT leak.
	if strings.Contains(itemBlock, "comment line should be skipped") {
		t.Errorf("comment line leaked into rendered example block")
	}
}

func TestRenderMarkdown_HiddenItemsExcluded(t *testing.T) {
	out := renderMD(t, Options{})
	if strings.Contains(out, "## `padtest secret`") {
		t.Errorf("hidden 'secret' command leaked into markdown output")
	}
	if strings.Contains(out, "internal-debug") {
		t.Errorf("hidden flag --internal-debug leaked into markdown output")
	}
}

func TestRenderMarkdown_DeterministicCommandOrder(t *testing.T) {
	// Two consecutive renders with the same fixed clock must produce
	// byte-identical output (commands sorted by path).
	a := renderMD(t, Options{})
	b := renderMD(t, Options{})
	if a != b {
		t.Errorf("two renders with the same fixed clock must be byte-identical (commands not sorted?)\n--- A ---\n%s\n--- B ---\n%s", head(a, 20), head(b, 20))
	}
}

func TestRenderMarkdown_StdinSection(t *testing.T) {
	// Build a small Document directly and verify the Stdin section
	// renders. We don't use the synthetic tree because cobra has no
	// machine-readable hint that a command accepts stdin; the Stdin
	// field on Command is populated explicitly by callers (TASK-936/938).
	doc := &Document{
		CmdhelpVersion: Version,
		Binary:         "demo",
		Commands: map[string]Command{
			"create": {
				Summary: "create",
				Stdin:   &Stdin{Accepted: true, Format: "text/markdown"},
			},
		},
	}
	var buf bytes.Buffer
	if err := RenderMarkdown(doc, Options{Now: fixedClock}, &buf); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "### Stdin") {
		t.Errorf("Stdin section missing")
	}
	if !strings.Contains(out, "text/markdown") {
		t.Errorf("Stdin format hint missing")
	}
}

func TestRenderMarkdown_OutputAndExitCodesSections(t *testing.T) {
	doc := &Document{
		CmdhelpVersion: Version,
		Binary:         "demo",
		Commands: map[string]Command{
			"create": {
				Summary: "create",
				Stdout:  &Stdout{TextTemplate: "Created {ref}", JSONSchemaRef: "#/schemas/Item"},
				ExitCodes: map[string]ExitCode{
					"0": {Description: "ok"},
					"2": {When: "validation error", Recovery: "Check args"},
				},
			},
		},
	}
	var buf bytes.Buffer
	if err := RenderMarkdown(doc, Options{Now: fixedClock}, &buf); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "### Output") {
		t.Errorf("Output section missing")
	}
	if !strings.Contains(out, "Created {ref}") {
		t.Errorf("text_template missing from Output section")
	}
	if !strings.Contains(out, "#/schemas/Item") {
		t.Errorf("json_schema_ref missing from Output section")
	}
	if !strings.Contains(out, "### Exit codes") {
		t.Errorf("Exit codes section missing")
	}
	if !strings.Contains(out, "validation error") {
		t.Errorf("Exit codes table missing 'when' for code 2")
	}
}

func TestRenderMarkdown_WorkspaceContextSection(t *testing.T) {
	doc := &Document{
		CmdhelpVersion: Version,
		Binary:         "demo",
		Commands:       map[string]Command{},
		Context:        &Context{Workspace: "docapp", Profile: "default"},
	}
	var buf bytes.Buffer
	if err := RenderMarkdown(doc, Options{Now: fixedClock}, &buf); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "## Workspace context") {
		t.Errorf("Workspace context section missing when Context is populated")
	}
	if !strings.Contains(out, "workspace: `docapp`") {
		t.Errorf("workspace value missing")
	}
}

func TestRenderMarkdown_SeeAlso(t *testing.T) {
	doc := &Document{
		CmdhelpVersion: Version,
		Binary:         "demo",
		Commands: map[string]Command{
			"create": {
				Summary: "create",
				SeeAlso: []string{"item update", "item delete"},
			},
		},
	}
	var buf bytes.Buffer
	if err := RenderMarkdown(doc, Options{Now: fixedClock}, &buf); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "### See also") {
		t.Errorf("See also section missing")
	}
	if !strings.Contains(out, "`item update`") || !strings.Contains(out, "`item delete`") {
		t.Errorf("see_also entries missing from output")
	}
}

func TestRenderMarkdown_EscapesPipeInTableCell(t *testing.T) {
	// Flag descriptions containing `|` would otherwise break a markdown
	// table; verify they're escaped.
	doc := &Document{
		CmdhelpVersion: Version,
		Binary:         "demo",
		Commands: map[string]Command{
			"f": {
				Summary: "flag stress",
				Flags: map[string]Flag{
					"weird": {Type: "string", Description: "value | with | pipes"},
				},
			},
		},
	}
	var buf bytes.Buffer
	if err := RenderMarkdown(doc, Options{Now: fixedClock}, &buf); err != nil {
		t.Fatalf("RenderMarkdown: %v", err)
	}
	if strings.Contains(buf.String(), "value | with | pipes") {
		t.Errorf("pipes in description not escaped — table grid will break")
	}
	if !strings.Contains(buf.String(), `value \| with \| pipes`) {
		t.Errorf("expected escaped pipes in description")
	}
}

// extractCommandBlock returns the substring of `md` covering one
// command's section, identified by its full path (e.g. "item create").
// Used to scope assertions to a single command rather than the whole
// document.
func extractCommandBlock(md, path string) string {
	heading := "## `padtest " + path + "`"
	idx := strings.Index(md, heading)
	if idx < 0 {
		return ""
	}
	rest := md[idx+len(heading):]
	// Block ends at the next `## ` heading at the same level.
	next := regexp.MustCompile(`(?m)^## `)
	if loc := next.FindStringIndex(rest); loc != nil {
		return md[idx : idx+len(heading)+loc[0]]
	}
	return md[idx:]
}

func head(s string, lines int) string {
	parts := strings.SplitN(s, "\n", lines+1)
	if len(parts) > lines {
		parts = parts[:lines]
	}
	return strings.Join(parts, "\n")
}
