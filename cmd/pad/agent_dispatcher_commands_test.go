package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/spf13/cobra"
)

// 1. RECON-AND-PLAN (TASK-3096; no interactive checkpoint in this harness,
// so plan and proceed — deviations flagged in the report).
//
// Recon:
//   - Dispatcher: agentsSkillBody in internal/cli/agents.go (unexported const,
//     two fenced ```bash blocks: "Common commands" + "Load details").
//     cli.FormatForTool(agents-tool, anything) returns frontmatter + body, so
//     the test reads the dispatcher through FormatForTool — no new export.
//   - Pinned: TestAgentDispatcherGuideTopicsResolve (cmd/pad/agent_guide_test.go),
//     TestAgentSkillPromptBudget (internal/cli/agent_prompt_budget_test.go).
//   - Cobra tree: newRootCmd() in cmd/pad/main.go, package main. A test in
//     internal/cli CANNOT use it (cmd/pad imports internal/cli — reverse import
//     would be a cycle). So this test lives in cmd/pad beside the guide-topics
//     pin. That is the one deliberate placement deviation.
//   - Prior art: internal/cmdhelp/example_validation.go (ValidateExamples:
//     root.Find for path resolution + flagExists walk up parents). This test
//     mirrors that logic locally rather than importing cmdhelp, because
//     cmdhelp.shellSplit/flagExists are unexported and dispatcher lines carry
//     `#` trailing comments plus placeholder tokens the cmdhelp validator
//     was never taught to skip.
//
// Plan:
//   - Extract ```bash fences from the formatted dispatcher with strict fence
//     pairing (an unterminated block, a stray close, or a nested open fails),
//     and require exactly expectedDispatcherBashBlocks blocks: a deleted or
//     truncated block must fail instead of passing on surviving commands.
//     One command per line, strip trailing `#` comments, shell-split quotes
//     (strict: a line with an unterminated quote or trailing backslash fails).
//   - Resolve the command path with placeholder tokens intact; strip
//     placeholders (<...>, [...], TASK-N) only from the trailing arguments
//     left after resolution, so a placeholder inside the command path fails.
//   - root.Find(tokens) must resolve past the bare root to a real subcommand;
//     every --flag (stripped of =value) ANYWHERE in the line — including
//     before the command path, where cobra's stripFlags would otherwise
//     swallow it during resolution — must be declared on the resolved target
//     or an ancestor (covers persistent --format/--workspace/--url).
//   - When the resolved target is a command group, every unconsumed trailing
//     token must be a flag, a flag value, or past `--`: anything else is a
//     typo'd subcommand (e.g. `pad item commment TASK-5` must not false-pass
//     as `pad item`). Placeholders do NOT consume a group — `pad item <ref>`
//     resolves to `pad item` with ["<ref>"] left over and must fail, or a
//     deleted subcommand could false-pass behind a placeholder. Leaf
//     commands keep accepting positional args and placeholders.
//   - Fail on zero parsed commands (guard against fence-format drift).
//   - Pin the required command inventory per block (expectedDispatcherCommands
//     + checkDispatcherInventory, enforced by
//     TestAgentDispatcherCommandsInventory): exact per-block set equality
//     plus an order check, so a deleted or substituted load-bearing command
//     fails even though every surviving line still resolves. A permanent
//     negative test proves deletion and substitution fail.

// expectedDispatcherBashBlocks pins the dispatcher's fenced bash block count:
// "Common commands" + "Load details". A deleted second block must fail here
// instead of passing on the surviving block's commands.
const expectedDispatcherBashBlocks = 2

// expectedDispatcherCommands pins the required command inventory per fenced
// bash block ("Common commands" + "Load details"), after stripShellComment +
// TrimSpace normalization with blank lines dropped. The resolve check in
// checkDispatcherLine validates only surviving lines, so a deleted or
// substituted load-bearing command would otherwise false-pass; exact
// per-block set equality plus an order check closes that hole. Update this
// inventory deliberately when the dispatcher in internal/cli/agents.go
// gains, loses, or rewrites a command.
var expectedDispatcherCommands = [][]string{
	{
		"pad project dashboard --format json",
		"pad item show TASK-5 --agent",
		"pad item list [collection] --format json",
		`pad item create <collection> "Title" [flags]`,
		"pad item update TASK-5 [flags]",
		`pad item comment TASK-5 "Message"`,
		"pad playbook list --format json",
		"pad playbook show <slug> --format markdown",
	},
	{
		"pad agent guide",
		"pad agent guide items",
		"pad agent guide before-performing-work",
		"pad agent guide role-awareness",
		"pad agent guide multi-step-workflows",
		"pad agent guide all",
	},
}

// normalizeDispatcherBlockLines returns the command lines of one fenced bash
// block body with trailing `#` comments removed, whitespace trimmed, and
// blank/comment-only lines dropped.
func normalizeDispatcherBlockLines(block string) []string {
	var out []string
	for _, raw := range strings.Split(block, "\n") {
		line := strings.TrimSpace(stripShellComment(raw))
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// checkDispatcherInventory compares the normalized per-block command lines
// against expectedDispatcherCommands, so a deleted, added, substituted, or
// reordered dispatcher command fails. It returns every problem found (empty
// means the inventory matches exactly).
func checkDispatcherInventory(blocks []string) []string {
	var problems []string
	if len(blocks) != len(expectedDispatcherCommands) {
		return []string{fmt.Sprintf("dispatcher inventory: expected %d blocks, got %d", len(expectedDispatcherCommands), len(blocks))}
	}
	for i, b := range blocks {
		got := normalizeDispatcherBlockLines(b)
		want := expectedDispatcherCommands[i]
		if len(got) != len(want) {
			problems = append(problems, fmt.Sprintf("dispatcher inventory block %d: expected %d commands, got %d", i+1, len(want), len(got)))
		}
		inGot := make(map[string]bool, len(got))
		for _, l := range got {
			inGot[l] = true
		}
		inWant := make(map[string]bool, len(want))
		for _, w := range want {
			inWant[w] = true
		}
		for _, w := range want {
			if !inGot[w] {
				problems = append(problems, fmt.Sprintf("dispatcher inventory block %d: missing command %q (deleted or substituted)", i+1, w))
			}
		}
		for _, l := range got {
			if !inWant[l] {
				problems = append(problems, fmt.Sprintf("dispatcher inventory block %d: unexpected command %q (added or substituted)", i+1, l))
			}
		}
		if len(got) == len(want) {
			for j := range want {
				if got[j] != want[j] {
					problems = append(problems, fmt.Sprintf("dispatcher inventory block %d: order drift at line %d: got %q, want %q", i+1, j+1, got[j], want[j]))
					break
				}
			}
		}
	}
	return problems
}

var dispatcherPlaceholder = regexp.MustCompile(`^(<[^>]*>|\[[^]]*\]|TASK-[0-9]+)$`)

// splitDispatcherBashBlocks returns the raw bodies of the dispatcher's fenced
// bash blocks, with strict fence pairing: a ```bash open must be closed by a
// following ``` line, a close with no open fails, a nested open fails, and an
// unterminated open at EOF fails. The block count must equal
// expectedDispatcherBashBlocks, and every block must contain at least one
// command line (ignoring blanks and `#` comment-only lines): an emptied
// block keeps both fences and the global command count but has lost
// load-bearing content, so it must fail here.
func splitDispatcherBashBlocks(body string) ([]string, error) {
	var blocks []string
	var cur []string
	inBlock := false
	for _, raw := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(raw)
		if !inBlock {
			if trim == "```bash" || strings.HasPrefix(trim, "```bash ") {
				inBlock = true
				cur = nil
				continue
			}
			if strings.HasPrefix(trim, "```") {
				return nil, fmt.Errorf("stray fence %q outside bash block %d", trim, len(blocks)+1)
			}
			continue
		}
		if trim == "```" {
			blocks = append(blocks, strings.Join(cur, "\n"))
			inBlock = false
			cur = nil
			continue
		}
		if strings.HasPrefix(trim, "```") {
			return nil, fmt.Errorf("unexpected fence %q inside bash block %d", trim, len(blocks)+1)
		}
		cur = append(cur, raw)
	}
	if inBlock {
		return nil, fmt.Errorf("unterminated fenced bash block %d", len(blocks)+1)
	}
	if len(blocks) != expectedDispatcherBashBlocks {
		return nil, fmt.Errorf("expected %d fenced bash blocks, got %d", expectedDispatcherBashBlocks, len(blocks))
	}
	for i, b := range blocks {
		hasCmd := false
		for _, raw := range strings.Split(b, "\n") {
			if strings.TrimSpace(stripShellComment(raw)) != "" {
				hasCmd = true
				break
			}
		}
		if !hasCmd {
			return nil, fmt.Errorf("fenced bash block %d contains no command lines", i+1)
		}
	}
	return blocks, nil
}

// dispatcherBashLines returns every command line from the dispatcher's fenced
// bash blocks, with trailing `#` comments removed.
func dispatcherBashLines(t *testing.T) []string {
	t.Helper()
	tool := cli.ResolveTool("agents")
	if tool == nil {
		t.Fatal(`ResolveTool("agents") returned nil`)
	}
	dispatcher := string(cli.FormatForTool(*tool, pad.PadSkill))
	blocks, err := splitDispatcherBashBlocks(dispatcher)
	if err != nil {
		t.Fatalf("dispatcher bash fences: %v", err)
	}
	var lines []string
	for _, b := range blocks {
		lines = append(lines, normalizeDispatcherBlockLines(b)...)
	}
	return lines
}

// stripShellComment cuts a trailing `#` comment, honouring single/double quotes.
func stripShellComment(s string) string {
	var inS, inD bool
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '#':
			if !inS && !inD {
				return s[:i]
			}
		}
	}
	return s
}

// dispatcherSplit tokenizes one command line POSIX-ish: whitespace separates,
// single/double quotes group, backslash escapes outside single quotes.
// It is strict: an unterminated quote or a trailing backslash is a malformed
// dispatcher line, so it returns an error instead of silently tokenizing.
func dispatcherSplit(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	var inS, inD, esc bool
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && !inS:
			esc = true
		case r == '"' && !inS:
			inD = !inD
		case r == '\'' && !inD:
			inS = !inS
		case (r == ' ' || r == '\t') && !inS && !inD:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	if esc {
		return nil, fmt.Errorf("trailing backslash")
	}
	if inS || inD {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return tokens, nil
}

func dispatcherFlagExists(cmd *cobra.Command, name string) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Flags().Lookup(name) != nil || c.PersistentFlags().Lookup(name) != nil {
			return true
		}
		if len(name) == 1 && (c.Flags().ShorthandLookup(name) != nil || c.PersistentFlags().ShorthandLookup(name) != nil) {
			return true
		}
	}
	return false
}

// dispatcherTrailingIsFlagValue reports whether a trailing token is the value
// consumed by the preceding --flag token. prev must be flag-like without `=`
// and either a known value-taking flag or an unknown flag (whose putative
// value is skipped to keep the error focused on the unknown-flag report).
// Known boolean flags (NoOptDefVal != "") take no value.
func dispatcherTrailingIsFlagValue(target *cobra.Command, prev string) bool {
	if prev == "--" || !strings.HasPrefix(prev, "-") || prev == "-" {
		return false
	}
	if strings.Contains(prev, "=") {
		return false
	}
	name := strings.TrimLeft(prev, "-")
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "no-") && dispatcherFlagExists(target, strings.TrimPrefix(name, "no-")) {
		return false
	}
	for c := target; c != nil; c = c.Parent() {
		if f := c.Flags().Lookup(name); f != nil {
			return f.NoOptDefVal == ""
		}
		if f := c.PersistentFlags().Lookup(name); f != nil {
			return f.NoOptDefVal == ""
		}
		if len(name) == 1 {
			if f := c.Flags().ShorthandLookup(name); f != nil {
				return f.NoOptDefVal == ""
			}
			if f := c.PersistentFlags().ShorthandLookup(name); f != nil {
				return f.NoOptDefVal == ""
			}
		}
	}
	return true
}

// checkDispatcherLine validates one dispatcher command line against the cobra
// tree and returns every problem found (empty means the line resolves cleanly).
func checkDispatcherLine(root *cobra.Command, line string) []string {
	var problems []string
	tokens, err := dispatcherSplit(line)
	if err != nil {
		return []string{fmt.Sprintf("dispatcher line %q: malformed (%v)", line, err)}
	}
	if len(tokens) == 0 {
		return nil
	}
	if tokens[0] != "pad" {
		return []string{fmt.Sprintf("dispatcher line %q does not start with pad", line)}
	}
	// Resolve with placeholder tokens intact, so a placeholder inside
	// the command path cannot silently vanish before Find. Only strip
	// placeholders from the trailing arguments left after resolution.
	args := tokens[1:]
	target, trailing, err := root.Find(args)
	if err != nil || target == nil || target == root {
		problems = append(problems, fmt.Sprintf("dispatcher line %q: command path does not resolve (%v)", line, err))
		return problems
	}
	consumed := len(args) - len(trailing)
	if consumed < 0 {
		consumed = 0
	}
	if consumed > len(args) {
		consumed = len(args)
	}
	for _, tok := range args[:consumed] {
		if dispatcherPlaceholder.MatchString(tok) {
			problems = append(problems, fmt.Sprintf("dispatcher line %q: placeholder %q inside command path", line, tok))
		}
	}
	// Re-resolve with placeholders removed: if stripping lets Find
	// reach a different command, a placeholder was interleaved inside
	// the command path (e.g. `pad item <id> show` must not pass as
	// `pad item`).
	var stripped []string
	for _, tok := range args {
		if dispatcherPlaceholder.MatchString(tok) {
			continue
		}
		stripped = append(stripped, tok)
	}
	if deeper, _, stripErr := root.Find(stripped); stripErr != nil {
		problems = append(problems, fmt.Sprintf("dispatcher line %q: placeholder-stripped path does not resolve (%v)", line, stripErr))
	} else if deeper != target {
		problems = append(problems, fmt.Sprintf("dispatcher line %q: placeholder inside command path (resolves to %q with placeholders, %q without)",
			line, target.CommandPath(), deeper.CommandPath()))
	}
	// Validate flags across the FULL line, not just the trailing args:
	// cobra's Find strips flag tokens during path resolution, so an unknown
	// flag placed before the command path (e.g. `pad --bogus item show`)
	// would otherwise never enter the trailing-args check. Checking against
	// the resolved target covers root persistent flags via the parent walk
	// in dispatcherFlagExists.
	flagToks := make([]string, 0, len(args))
	for _, tok := range args {
		if dispatcherPlaceholder.MatchString(tok) {
			continue
		}
		if !strings.HasPrefix(tok, "-") || tok == "--" {
			continue
		}
		flagToks = append(flagToks, tok)
	}
	for _, tok := range flagToks {
		name := strings.TrimLeft(tok, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			continue
		}
		if !dispatcherFlagExists(target, name) {
			if strings.HasPrefix(name, "no-") && dispatcherFlagExists(target, strings.TrimPrefix(name, "no-")) {
				continue
			}
			problems = append(problems, fmt.Sprintf("dispatcher line %q: unknown flag --%s on %q", line, name, target.CommandPath()))
		}
	}
	// Fail on unconsumed trailing tokens when the resolved target is a
	// command group (e.g. `pad item commment TASK-5` resolves to `pad item`
	// with ["commment" "TASK-5"] left over, and must not false-pass as
	// `pad item`). Leaf commands accept positional args, so only groups
	// fail here. Flags (validated above), flag values, and tokens after
	// `--` pass through; placeholders do NOT consume a group — a bare
	// `pad item <ref>` (or TASK-5 / [collection]) left over after resolving
	// to a group means a deleted subcommand could false-pass, so it fails.
	// A placeholder consumed as a flag value (e.g. `--format <fmt>`) still
	// passes via the flag-value check below.
	if len(target.Commands()) > 0 {
		seenDashDash := false
		for i, tok := range trailing {
			if tok == "--" {
				seenDashDash = true
				continue
			}
			if !seenDashDash && strings.HasPrefix(tok, "-") && tok != "-" {
				continue
			}
			if !seenDashDash && i > 0 && dispatcherTrailingIsFlagValue(target, trailing[i-1]) {
				continue
			}
			if dispatcherPlaceholder.MatchString(tok) {
				problems = append(problems, fmt.Sprintf("dispatcher line %q: placeholder %q does not consume group %q (unknown command)", line, tok, target.CommandPath()))
				continue
			}
			problems = append(problems, fmt.Sprintf("dispatcher line %q: unknown command %q for %q", line, tok, target.CommandPath()))
		}
	}
	return problems
}

func TestAgentDispatcherCommandsResolve(t *testing.T) {
	root := newRootCmd()
	lines := dispatcherBashLines(t)
	if len(lines) == 0 {
		t.Fatal("dispatcher bash blocks contain no command lines")
	}
	for _, line := range lines {
		for _, p := range checkDispatcherLine(root, line) {
			t.Error(p)
		}
	}
}

// TestDispatcherLineRejectsUnknownPrePathFlag is the negative control for the
// pre-path flag check: an unknown flag before the command path must fail even
// though cobra's Find strips it during path resolution.
func TestDispatcherLineRejectsUnknownPrePathFlag(t *testing.T) {
	root := newRootCmd()
	// = form: cobra's stripFlags leaves it alone during resolution, so only
	// the full-line flag check can reject it — assert the flag error itself.
	badEq := "pad --bogus-prepath-xyz=json item show TASK-5"
	problems := checkDispatcherLine(root, badEq)
	found := false
	for _, p := range problems {
		if strings.Contains(p, "unknown flag --bogus-prepath-xyz") {
			found = true
		}
	}
	if !found {
		t.Fatalf("pre-path unknown flag %q not reported as unknown flag: %v", badEq, problems)
	}
	// A known persistent root flag before the command path must still pass.
	good := "pad --format json item show TASK-5"
	if problems := checkDispatcherLine(root, good); len(problems) != 0 {
		t.Fatalf("known pre-path persistent flag %q rejected: %v", good, problems)
	}
}

// TestDispatcherFencePairingRejects is the negative control for the fence-count
// pin: a deleted second block or a truncated (unterminated) block must fail
// instead of passing on surviving commands.
func TestDispatcherFencePairingRejects(t *testing.T) {
	oneBlock := "# Pad\n\n```bash\npad item show TASK-5\n```\n"
	if _, err := splitDispatcherBashBlocks(oneBlock); err == nil {
		t.Fatal("single bash block passed; want exactly 2-block pin to fail")
	}
	truncated := "# Pad\n\n```bash\npad item show TASK-5\n```\n\n```bash\npad agent guide\n"
	if _, err := splitDispatcherBashBlocks(truncated); err == nil {
		t.Fatal("unterminated second bash block passed; want strict pairing to fail")
	}
	strayClose := "# Pad\n\n```\n\n```bash\npad item show TASK-5\n```\n\n```bash\npad agent guide\n```\n"
	if _, err := splitDispatcherBashBlocks(strayClose); err == nil {
		t.Fatal("stray close fence outside bash block passed; want strict pairing to fail")
	}
	nestedOpen := "# Pad\n\n```bash\npad item show TASK-5\n```bash\n```\n\n```bash\npad agent guide\n```\n"
	if _, err := splitDispatcherBashBlocks(nestedOpen); err == nil {
		t.Fatal("nested fence open inside bash block passed; want strict pairing to fail")
	}
	emptySecond := "# Pad\n\n```bash\npad item show TASK-5\n```\n\n```bash\n\n```\n"
	if _, err := splitDispatcherBashBlocks(emptySecond); err == nil {
		t.Fatal("emptied second bash block passed; want per-block non-empty guard to fail")
	}
	commentOnly := "# Pad\n\n```bash\npad item show TASK-5\n```\n\n```bash\n# only a comment\n```\n"
	if _, err := splitDispatcherBashBlocks(commentOnly); err == nil {
		t.Fatal("comment-only second bash block passed; want per-block non-empty guard to fail")
	}
	emptyFirst := "# Pad\n\n```bash\n   \n```\n\n```bash\npad agent guide\n```\n"
	if _, err := splitDispatcherBashBlocks(emptyFirst); err == nil {
		t.Fatal("emptied first bash block passed; want per-block non-empty guard to fail")
	}
}

// TestDispatcherSplitRejectsMalformed is the negative control for the
// tokenizer strictness logic: an unterminated quote or a trailing backslash
// is a malformed dispatcher line, so dispatcherSplit must error and
// checkDispatcherLine must report it instead of silently tokenizing.
func TestDispatcherSplitRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		`pad item show "unterminated`,
		`pad item show 'unterminated`,
		`pad item show foo\`,
	} {
		if _, err := dispatcherSplit(bad); err == nil {
			t.Errorf("dispatcherSplit(%q) passed; want malformed-line error", bad)
		}
		problems := checkDispatcherLine(newRootCmd(), bad)
		found := false
		for _, p := range problems {
			if strings.Contains(p, "malformed") {
				found = true
			}
		}
		if !found {
			t.Errorf("checkDispatcherLine(%q) = %v; want a malformed-line problem", bad, problems)
		}
	}
	// A well-formed quoted token still tokenizes and is not flagged malformed.
	toks, err := dispatcherSplit(`pad item show "TASK-5"`)
	if err != nil {
		t.Fatalf("dispatcherSplit quoted line failed: %v", err)
	}
	if len(toks) != 4 || toks[3] != "TASK-5" {
		t.Fatalf("dispatcherSplit quoted line = %q; want [pad item show TASK-5]", toks)
	}
}

// TestDispatcherLineRejectsPlaceholderInPath is the negative control for the
// placeholder-path re-resolution logic: a placeholder interleaved inside the
// command path must fail, while the same placeholder as a trailing argument
// must pass.
func TestDispatcherLineRejectsPlaceholderInPath(t *testing.T) {
	root := newRootCmd()
	bad := "pad item <id> show"
	problems := checkDispatcherLine(root, bad)
	if len(problems) == 0 {
		t.Fatalf("placeholder-in-path line %q passed; want rejection", bad)
	}
	found := false
	for _, p := range problems {
		if strings.Contains(p, "placeholder") {
			found = true
		}
	}
	if !found {
		t.Fatalf("placeholder-in-path line %q problems %v mention no placeholder", bad, problems)
	}
	good := "pad item show TASK-5"
	if problems := checkDispatcherLine(root, good); len(problems) != 0 {
		t.Fatalf("trailing-placeholder line %q rejected: %v", good, problems)
	}
}

// TestDispatcherLineRejectsTypoSubcommand is the negative control for the
// unconsumed-trailing-token check: a typo'd subcommand that cobra's Find
// leaves unconsumed (resolving to the parent group) must fail instead of
// false-passing as the group, while legit trailing args, placeholders,
// flags, and flag values keep passing.
func TestDispatcherLineRejectsTypoSubcommand(t *testing.T) {
	root := newRootCmd()
	for _, bad := range []string{
		"pad item commment TASK-5",
		"pad item showw TASK-5",
		"pad item commment TASK-5 --format json",
	} {
		problems := checkDispatcherLine(root, bad)
		found := false
		for _, p := range problems {
			if strings.Contains(p, "unknown command") {
				found = true
			}
		}
		if !found {
			t.Errorf("typo'd subcommand line %q passed or missed unknown-command; problems: %v", bad, problems)
		}
	}
	// Legit lines must still pass: leaf positionals (literal and
	// placeholder), topic args, flags, and group-with-flags-only.
	for _, good := range []string{
		"pad item show TASK-5",
		`pad item comment TASK-5 "Message"`,
		`pad item create <collection> "Title" [flags]`,
		"pad item list [collection] --format json",
		"pad agent guide items",
		"pad project dashboard --format json",
		"pad item --format json",
	} {
		if problems := checkDispatcherLine(root, good); len(problems) != 0 {
			t.Errorf("legit line %q rejected: %v", good, problems)
		}
	}
}

// TestDispatcherLineRejectsPlaceholderOnGroup is the negative control for the
// placeholder-on-group hole: a placeholder-shaped trailing token must NOT
// count as consuming a command group. `pad item <ref>` resolves to the `pad
// item` group with ["<ref>"] left over and must fail (otherwise a deleted
// subcommand could false-pass behind a placeholder), while legit leaf
// placeholder positionals keep passing.
func TestDispatcherLineRejectsPlaceholderOnGroup(t *testing.T) {
	root := newRootCmd()
	for _, bad := range []string{
		"pad item <ref>",
		"pad item <id>",
		"pad item TASK-5",
		"pad item [collection]",
		"pad item <ref> --format json",
	} {
		problems := checkDispatcherLine(root, bad)
		if len(problems) == 0 {
			t.Errorf("placeholder-on-group line %q passed; want rejection", bad)
			continue
		}
		found := false
		for _, p := range problems {
			if strings.Contains(p, "placeholder") {
				found = true
			}
		}
		if !found {
			t.Errorf("placeholder-on-group line %q problems %v mention no placeholder", bad, problems)
		}
	}
	// Legit leaf placeholder positionals and group-with-flags-only must pass.
	for _, good := range []string{
		"pad item create <collection>",
		`pad item create <collection> "Title" [flags]`,
		"pad item show TASK-5",
		"pad item list [collection] --format json",
		"pad playbook show <slug> --format markdown",
		"pad item --format json",
	} {
		if problems := checkDispatcherLine(root, good); len(problems) != 0 {
			t.Errorf("legit placeholder line %q rejected: %v", good, problems)
		}
	}
}

// TestAgentDispatcherCommandsInventory pins the required command inventory:
// the normalized per-block command lines must equal
// expectedDispatcherCommands exactly, so a deleted or substituted
// load-bearing command fails even though every surviving line still resolves
// against the cobra tree.
func TestAgentDispatcherCommandsInventory(t *testing.T) {
	tool := cli.ResolveTool("agents")
	if tool == nil {
		t.Fatal(`ResolveTool("agents") returned nil`)
	}
	dispatcher := string(cli.FormatForTool(*tool, pad.PadSkill))
	blocks, err := splitDispatcherBashBlocks(dispatcher)
	if err != nil {
		t.Fatalf("dispatcher bash fences: %v", err)
	}
	for _, p := range checkDispatcherInventory(blocks) {
		t.Error(p)
	}
}

// TestDispatcherInventoryRejectsDeletionOrSubstitution is the permanent
// negative control for the command-inventory pin: a deleted or substituted
// dispatcher command must fail the inventory check even though every
// surviving line still resolves (which is exactly why the resolve-only pin
// false-passed before this inventory existed).
func TestDispatcherInventoryRejectsDeletionOrSubstitution(t *testing.T) {
	join := func(lines []string) string { return strings.Join(lines, "\n") }
	wrap := func(blocks []string) []string {
		body := "# Pad\n\n```bash\n" + blocks[0] + "\n```\n\n```bash\n" + blocks[1] + "\n```\n"
		parsed, err := splitDispatcherBashBlocks(body)
		if err != nil {
			t.Fatalf("test helper failed to reparse wrapped blocks: %v", err)
		}
		return parsed
	}
	contains := func(problems []string, want string) bool {
		for _, p := range problems {
			if strings.Contains(p, want) {
				return true
			}
		}
		return false
	}

	// Deletion in block 1: drop the comment command. Every surviving line
	// still resolves, so the resolve-only pin would pass — assert that first
	// to prove the hole is real, then require the inventory to fail.
	delLines := append([]string{}, expectedDispatcherCommands[0][:5]...)
	delLines = append(delLines, expectedDispatcherCommands[0][6:]...)
	root := newRootCmd()
	for _, line := range delLines {
		if problems := checkDispatcherLine(root, line); len(problems) != 0 {
			t.Fatalf("surviving line %q unexpectedly fails to resolve: %v", line, problems)
		}
	}
	deleted := wrap([]string{join(delLines), join(expectedDispatcherCommands[1])})
	delProblems := checkDispatcherInventory(deleted)
	if len(delProblems) == 0 {
		t.Fatal("deleted command passed inventory; want failure")
	}
	if !contains(delProblems, `missing command "pad item comment TASK-5 \"Message\""`) {
		t.Errorf("deleted-command problems %v do not name the missing command", delProblems)
	}

	// Deletion in block 2: drop one guide topic line.
	del2Lines := append([]string{}, expectedDispatcherCommands[1][:2]...)
	del2Lines = append(del2Lines, expectedDispatcherCommands[1][3:]...)
	deleted2 := wrap([]string{join(expectedDispatcherCommands[0]), join(del2Lines)})
	del2Problems := checkDispatcherInventory(deleted2)
	if len(del2Problems) == 0 {
		t.Fatal("deleted block-2 command passed inventory; want failure")
	}
	if !contains(del2Problems, "missing command") {
		t.Errorf("deleted block-2 problems %v mention no missing command", del2Problems)
	}

	// Substitution in block 1: replace the comment command with a duplicate
	// of a surviving line. Same count, every line resolves — only the
	// inventory (missing + order drift) can catch it.
	subLines := append([]string{}, expectedDispatcherCommands[0]...)
	subLines[5] = expectedDispatcherCommands[0][1]
	for _, line := range subLines {
		if problems := checkDispatcherLine(root, line); len(problems) != 0 {
			t.Fatalf("substituted-set line %q unexpectedly fails to resolve: %v", line, problems)
		}
	}
	substituted := wrap([]string{join(subLines), join(expectedDispatcherCommands[1])})
	subProblems := checkDispatcherInventory(substituted)
	if len(subProblems) == 0 {
		t.Fatal("substituted command passed inventory; want failure")
	}
	if !contains(subProblems, "missing command") {
		t.Errorf("substituted-command problems %v mention no missing command", subProblems)
	}

	// Substitution in block 2 with a resolving-but-unexpected line.
	sub2Lines := append([]string{}, expectedDispatcherCommands[1]...)
	sub2Lines[1] = "pad agent guide"
	substituted2 := wrap([]string{join(expectedDispatcherCommands[0]), join(sub2Lines)})
	sub2Problems := checkDispatcherInventory(substituted2)
	if len(sub2Problems) == 0 {
		t.Fatal("substituted block-2 command passed inventory; want failure")
	}
	if !contains(sub2Problems, "unexpected command") && !contains(sub2Problems, "missing command") {
		t.Errorf("substituted block-2 problems %v mention neither missing nor unexpected command", sub2Problems)
	}

	// The pristine inventory must pass: guards the negative test against
	// rotting (failing on the real dispatcher for the wrong reason).
	pristine := wrap([]string{join(expectedDispatcherCommands[0]), join(expectedDispatcherCommands[1])})
	if problems := checkDispatcherInventory(pristine); len(problems) != 0 {
		t.Fatalf("pristine inventory rejected: %v", problems)
	}
}
