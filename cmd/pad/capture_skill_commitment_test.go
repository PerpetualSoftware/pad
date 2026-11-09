package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// The capture skill teaches an agent to turn one sentence into a commitment
// with `pad remind`. These tests keep the skill honest against the real
// command tree: every `pad remind ...` line it shows must resolve to a real
// command and carry only flags that command declares, and each scenario the
// skill walks through must produce the payload the sentence calls for.

const captureSkillPath = "../../plugin/skills/capture/SKILL.md"

func captureSkill(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(captureSkillPath)
	if err != nil {
		t.Fatalf("read capture skill: %v", err)
	}
	return string(b)
}

// remindLines returns every shell line in the skill that starts with
// `pad remind`, in order, with any trailing `# comment` removed.
func remindLines(skill string) []string {
	var out []string
	for _, l := range strings.Split(skill, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "pad remind") {
			continue
		}
		if i := strings.Index(l, "  #"); i >= 0 {
			l = strings.TrimSpace(l[:i])
		}
		out = append(out, l)
	}
	return out
}

// payload parses one skill command against the real tree and returns the
// subcommand path and the flags that were set.
func payload(t *testing.T, line string) (string, map[string]string) {
	t.Helper()
	// Skill lines quote only a single -m value; fields with quotes are
	// rejoined so the comment stays one argument.
	args := splitQuoted(line)
	root := newRootCmd()
	cmd, rest, err := root.Find(args[1:])
	if err != nil {
		t.Fatalf("%q: no such command: %v", line, err)
	}
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatalf("%q: flag not declared by %s: %v", line, cmd.CommandPath(), err)
	}
	set := map[string]string{}
	cmd.Flags().Visit(func(f *pflag.Flag) { set[f.Name] = f.Value.String() })
	return cmd.CommandPath(), set
}

func TestCaptureSkillCommandsResolveAgainstTheRealTree(t *testing.T) {
	lines := remindLines(captureSkill(t))
	if len(lines) == 0 {
		t.Fatal("capture skill shows no `pad remind` command")
	}
	for _, l := range lines {
		payload(t, l)
	}
}

func TestCaptureSkillScenarios(t *testing.T) {
	skill := captureSkill(t)
	cases := []struct {
		name     string
		contains string // a command line the skill must show for this sentence
		path     string
		flags    map[string]string
	}{
		{"simple trash: one sentence, target only", "--every weekly", "pad remind",
			map[string]string{"start": "", "every": "weekly"}},
		{"deadline lapses vs stays owed", "--deadline hard", "pad remind",
			map[string]string{"start": "", "deadline": "hard"}},
		{"completed yesterday", "pad remind done", "pad remind done",
			map[string]string{"at": ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var found string
			for _, l := range remindLines(skill) {
				if strings.Contains(l, c.contains) {
					found = l
					break
				}
			}
			if found == "" {
				t.Fatalf("skill shows no command containing %q", c.contains)
			}
			path, set := payload(t, found)
			if path != c.path {
				t.Fatalf("%q resolved to %q, want %q", found, path, c.path)
			}
			for k := range c.flags {
				if _, ok := set[k]; !ok {
					t.Errorf("%q does not set --%s", found, k)
				}
			}
		})
	}
}

func TestCaptureSkillRules(t *testing.T) {
	// Collapse line wraps so a pattern can span them.
	skill := strings.Join(strings.Fields(captureSkill(t)), " ")
	lower := strings.ToLower(skill)

	must := map[string]string{
		"labels target vs deadline in the read-back": `(?i)target.{0,80}deadline|deadline.{0,80}target`,
		"asks only what changes behaviour":           `(?i)only.{0,60}(changes|change) (the )?behaviou?r`,
		"reports links as unavailable":               `(?i)links? (are|is) not available|links? unavailable`,
		"keeps the autonomous-capture exception":     `(?i)autonomous`,
		"keeps the convention load":                  `trigger=always`,
		"offers an edit after the read-back":         `(?i)read.?back`,
	}
	for name, re := range must {
		if !regexp.MustCompile(re).MatchString(skill) {
			t.Errorf("capture skill does not %s (pattern %s)", name, re)
		}
	}

	// Things the rulings forbid.
	for _, bad := range []string{"pad cal", "t-shirt", "story point", "handler:"} {
		if strings.Contains(lower, bad) {
			t.Errorf("capture skill mentions %q", bad)
		}
	}
	if regexp.MustCompile(`(?i)how big|what size|size of (the|this) (task|chore)`).MatchString(skill) {
		t.Error("capture skill prompts for a size")
	}
}

func splitQuoted(line string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
