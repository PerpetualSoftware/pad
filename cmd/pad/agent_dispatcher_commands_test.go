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
//   - Extract ```bash fences from the formatted dispatcher, one command per
//     line, strip trailing `#` comments, shell-split quotes (strict: a line
//     with an unterminated quote or trailing backslash fails).
//   - Resolve the command path with placeholder tokens intact; strip
//     placeholders (<...>, [...], TASK-N) only from the trailing arguments
//     left after resolution, so a placeholder inside the command path fails.
//   - root.Find(tokens) must resolve past the bare root to a real subcommand;
//     every --flag (stripped of =value) must be declared on the target or an
//     ancestor (covers persistent --format/--workspace/--url).
//   - Fail on zero parsed commands (guard against fence-format drift).

var dispatcherBashFence = regexp.MustCompile("(?s)```bash(.*?)```")
var dispatcherPlaceholder = regexp.MustCompile(`^(<[^>]*>|\[[^]]*\]|TASK-[0-9]+)$`)

// dispatcherBashLines returns every command line from the dispatcher's fenced
// bash blocks, with trailing `#` comments removed.
func dispatcherBashLines(t *testing.T) []string {
	t.Helper()
	tool := cli.ResolveTool("agents")
	if tool == nil {
		t.Fatal(`ResolveTool("agents") returned nil`)
	}
	dispatcher := string(cli.FormatForTool(*tool, pad.PadSkill))
	fences := dispatcherBashFence.FindAllStringSubmatch(dispatcher, -1)
	if len(fences) == 0 {
		t.Fatal("dispatcher contains no fenced bash blocks")
	}
	var lines []string
	for _, f := range fences {
		for _, raw := range strings.Split(f[1], "\n") {
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

func TestAgentDispatcherCommandsResolve(t *testing.T) {
	root := newRootCmd()
	lines := dispatcherBashLines(t)
	if len(lines) == 0 {
		t.Fatal("dispatcher bash blocks contain no command lines")
	}
	for _, line := range lines {
		tokens, err := dispatcherSplit(line)
		if err != nil {
			t.Errorf("dispatcher line %q: malformed (%v)", line, err)
			continue
		}
		if len(tokens) == 0 {
			continue
		}
		if tokens[0] != "pad" {
			t.Errorf("dispatcher line %q does not start with pad", line)
			continue
		}
		// Resolve with placeholder tokens intact, so a placeholder inside
		// the command path cannot silently vanish before Find. Only strip
		// placeholders from the trailing arguments left after resolution.
		args := tokens[1:]
		target, trailing, err := root.Find(args)
		if err != nil || target == nil || target == root {
			t.Errorf("dispatcher line %q: command path does not resolve (%v)", line, err)
			continue
		}
		consumed := len(args) - len(trailing)
		for _, tok := range args[:consumed] {
			if dispatcherPlaceholder.MatchString(tok) {
				t.Errorf("dispatcher line %q: placeholder %q inside command path", line, tok)
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
			t.Errorf("dispatcher line %q: placeholder-stripped path does not resolve (%v)", line, stripErr)
		} else if deeper != target {
			t.Errorf("dispatcher line %q: placeholder inside command path (resolves to %q with placeholders, %q without)",
				line, target.CommandPath(), deeper.CommandPath())
		}
		var rest []string
		for _, tok := range trailing {
			if dispatcherPlaceholder.MatchString(tok) {
				continue
			}
			rest = append(rest, tok)
		}
		for _, tok := range rest {
			if !strings.HasPrefix(tok, "-") || tok == "--" {
				continue
			}
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
				t.Errorf("dispatcher line %q: unknown flag --%s on %q", line, name, target.CommandPath())
			}
		}
	}
}
