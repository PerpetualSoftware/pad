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
//   - Fail on zero parsed commands (guard against fence-format drift).

// expectedDispatcherBashBlocks pins the dispatcher's fenced bash block count:
// "Common commands" + "Load details". A deleted second block must fail here
// instead of passing on the surviving block's commands.
const expectedDispatcherBashBlocks = 2

var dispatcherPlaceholder = regexp.MustCompile(`^(<[^>]*>|\[[^]]*\]|TASK-[0-9]+)$`)

// splitDispatcherBashBlocks returns the raw bodies of the dispatcher's fenced
// bash blocks, with strict fence pairing: a ```bash open must be closed by a
// following ``` line, a close with no open fails, a nested open fails, and an
// unterminated open at EOF fails. The block count must equal
// expectedDispatcherBashBlocks.
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
		for _, raw := range strings.Split(b, "\n") {
			line := strings.TrimSpace(stripShellComment(raw))
			if line == "" {
				continue
			}
			lines = append(lines, line)
		}
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
}
