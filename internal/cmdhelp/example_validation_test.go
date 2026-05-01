package cmdhelp

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestShellSplit_BasicCases(t *testing.T) {
	cases := map[string][]string{
		`pad item create task "Fix OAuth"`: {"pad", "item", "create", "task", "Fix OAuth"},
		`pad item create idea 'one two'`:   {"pad", "item", "create", "idea", "one two"},
		`pad foo --bar=baz qux`:            {"pad", "foo", "--bar=baz", "qux"},
		`pad x \"escaped\"`:                {"pad", "x", `"escaped"`},
		`pad   item    create`:             {"pad", "item", "create"},
	}
	for in, want := range cases {
		got, err := shellSplit(in)
		if err != nil {
			t.Errorf("shellSplit(%q) error: %v", in, err)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("shellSplit(%q) = %v (len %d), want %v (len %d)", in, got, len(got), want, len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("shellSplit(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

func TestShellSplit_UnterminatedQuotesError(t *testing.T) {
	if _, err := shellSplit(`pad "unterminated`); err == nil {
		t.Errorf("expected error for unterminated double quote")
	}
	if _, err := shellSplit(`pad 'unterminated`); err == nil {
		t.Errorf("expected error for unterminated single quote")
	}
}

func TestValidateExamples_Clean(t *testing.T) {
	// Build a tree where the example references real commands and flags.
	root := &cobra.Command{Use: "padtest"}
	root.PersistentFlags().String("workspace", "", "workspace override")

	create := &cobra.Command{
		Use:     "create <coll>",
		Short:   "create item",
		Example: `  padtest item create task --priority high`,
	}
	create.Flags().String("priority", "", "priority")

	item := &cobra.Command{Use: "item", Short: "item group"}
	item.AddCommand(create)
	root.AddCommand(item)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	findings := ValidateExamples(doc, root)
	if len(findings) != 0 {
		t.Errorf("expected no findings, got: %v", findings)
	}
}

func TestValidateExamples_DetectsTypoFlag(t *testing.T) {
	// THIS IS THE DRIFT-PREVENTION CONTRACT TEST (spec §6 / §11 Q5).
	// A typo in an example flag — `--priorty` instead of `--priority`
	// — MUST be detected by ValidateExamples and reported with the
	// offending command + example index.
	root := &cobra.Command{Use: "padtest"}
	leaf := &cobra.Command{
		Use:     "leaf",
		Short:   "leaf",
		Example: `  padtest leaf --priorty high`, // typo
	}
	leaf.Flags().String("priority", "", "priority")
	root.AddCommand(leaf)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	findings := ValidateExamples(doc, root)

	if len(findings) == 0 {
		t.Fatalf("expected at least one finding for --priorty typo, got none")
	}
	want := []string{"leaf", "example[0]", "priorty"}
	for _, w := range want {
		if !strings.Contains(findings[0], w) {
			t.Errorf("finding should mention %q for triage; got: %q", w, findings[0])
		}
	}
}

func TestValidateExamples_DetectsUnknownCommand(t *testing.T) {
	// Example references a command path that doesn't exist on the tree.
	// Common when a command gets renamed/deleted but the example block
	// in Long stays stale.
	root := &cobra.Command{Use: "padtest"}
	leaf := &cobra.Command{
		Use:     "leaf",
		Short:   "leaf",
		Example: `  padtest does-not-exist --foo bar`,
	}
	root.AddCommand(leaf)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	findings := ValidateExamples(doc, root)
	if len(findings) == 0 {
		t.Errorf("expected finding for unknown command 'does-not-exist'")
	}
}

func TestValidateExamples_AcceptsNegateFlag(t *testing.T) {
	// `--no-cache` form must be accepted when the underlying flag is
	// `cache` (negation per spec §5.3).
	root := &cobra.Command{Use: "padtest"}
	leaf := &cobra.Command{
		Use:     "leaf",
		Short:   "leaf",
		Example: `  padtest leaf --no-cache`,
	}
	leaf.Flags().Bool("cache", true, "use cache")
	root.AddCommand(leaf)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	findings := ValidateExamples(doc, root)
	if len(findings) != 0 {
		t.Errorf("--no-cache should resolve via negate-flag rule, got findings: %v", findings)
	}
}

func TestValidateExamples_FlagBeforeSubcommandResolvesToCorrectTarget(t *testing.T) {
	// Cobra accepts flags interleaved with the command path — e.g.
	// `pad --workspace foo item create task --priority high`.
	// The validator MUST resolve to `item create` (not root) so leaf
	// flags like --priority are checked against the leaf, not just
	// against root's flag set. (Caught by Codex round 1.)
	root := &cobra.Command{Use: "padtest"}
	root.PersistentFlags().String("workspace", "", "workspace override")

	create := &cobra.Command{
		Use:     "create <coll>",
		Short:   "create",
		Example: `  padtest --workspace foo item create task --priority high`,
	}
	create.Flags().String("priority", "", "priority")

	item := &cobra.Command{Use: "item", Short: "item group"}
	item.AddCommand(create)
	root.AddCommand(item)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	if findings := ValidateExamples(doc, root); len(findings) != 0 {
		t.Errorf("flag-before-subcommand example should resolve cleanly; got: %v", findings)
	}
}

func TestValidateExamples_AcceptsPersistentFlagFromAncestor(t *testing.T) {
	// `--workspace` is a persistent root flag; it must be accepted on
	// any subcommand example.
	root := &cobra.Command{Use: "padtest"}
	root.PersistentFlags().String("workspace", "", "workspace override")
	leaf := &cobra.Command{
		Use:     "leaf",
		Short:   "leaf",
		Example: `  padtest leaf --workspace foo`,
	}
	root.AddCommand(leaf)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	if findings := ValidateExamples(doc, root); len(findings) != 0 {
		t.Errorf("persistent root flag should resolve on subcommand, got: %v", findings)
	}
}

func TestValidateExamples_SkipsNonBinaryExamples(t *testing.T) {
	// Documentation snippets that aren't pad invocations (e.g.
	// `cat ~/foo.json | jq` to show output) are skipped, not flagged.
	// The drift contract is for pad-invocation drift specifically;
	// non-pad lines are docs prose.
	root := &cobra.Command{Use: "padtest"}
	leaf := &cobra.Command{
		Use:   "leaf",
		Short: "leaf",
		Example: `  cat ~/.padtest/foo.json | jq
  padtest leaf --real-flag`,
	}
	leaf.Flags().Bool("real-flag", false, "real flag")
	root.AddCommand(leaf)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	findings := ValidateExamples(doc, root)
	if len(findings) != 0 {
		t.Errorf("non-pad example should be skipped silently; got: %v", findings)
	}
}

func TestValidateExamples_PipelineStopsAtFirstCommand(t *testing.T) {
	// `padtest leaf --foo | jq -r .x` — the validator only checks the
	// first command in the pipeline. The `-r .x` after the pipe
	// belongs to jq, not padtest, and would erroneously flag if we
	// kept tokenizing past `|`.
	root := &cobra.Command{Use: "padtest"}
	leaf := &cobra.Command{
		Use:     "leaf",
		Short:   "leaf",
		Example: `  padtest leaf --foo | jq -r .x`,
	}
	leaf.Flags().Bool("foo", false, "foo flag")
	root.AddCommand(leaf)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	if findings := ValidateExamples(doc, root); len(findings) != 0 {
		t.Errorf("pipeline should be honored; got: %v", findings)
	}
}

func TestParseExamplesFromLong_StripsSameLineComments(t *testing.T) {
	// Pad's existing convention: trailing `# annotation` on an example
	// line. Should be stripped before recording the example.
	long := `Examples:
  pad attachment list --item TASK-5    # one item's attachments
  pad attachment list --category image # filter by category`

	got := parseExamplesFromLong(long)
	if len(got) != 2 {
		t.Fatalf("expected 2 examples, got %d: %+v", len(got), got)
	}
	if strings.Contains(got[0].Cmd, "#") {
		t.Errorf("trailing comment leaked into example[0]: %q", got[0].Cmd)
	}
	if got[0].Cmd != "pad attachment list --item TASK-5" {
		t.Errorf("example[0] = %q, want stripped form", got[0].Cmd)
	}
}

func TestValidateBoolArity_DetectsValuedBool(t *testing.T) {
	// Spec §5.3: bool flags MUST be presence-only. An example using
	// `--cache=true` violates the contract.
	root := &cobra.Command{Use: "padtest"}
	leaf := &cobra.Command{
		Use:     "leaf",
		Short:   "leaf",
		Example: `  padtest leaf --cache=true`,
	}
	leaf.Flags().Bool("cache", false, "cache")
	root.AddCommand(leaf)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	findings := ValidateBoolArity(doc)
	if len(findings) == 0 {
		t.Errorf("expected violation for --cache=true on bool flag")
	}
	if len(findings) > 0 && !strings.Contains(findings[0], "cache") {
		t.Errorf("finding should mention the offending flag: %q", findings[0])
	}
}

func TestValidateBoolArity_AcceptsPresenceForm(t *testing.T) {
	root := &cobra.Command{Use: "padtest"}
	leaf := &cobra.Command{
		Use:     "leaf",
		Short:   "leaf",
		Example: `  padtest leaf --cache`,
	}
	leaf.Flags().Bool("cache", false, "cache")
	root.AddCommand(leaf)

	doc := Build(root, root, Options{Binary: "padtest", MaxDepth: -1})
	if findings := ValidateBoolArity(doc); len(findings) != 0 {
		t.Errorf("--cache (presence form) should be accepted, got: %v", findings)
	}
}
