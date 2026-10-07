package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-3466: what each door does with an existing skill file, decided once.

const testSkill = "---\nname: pad\ndescription: test\n---\n\n# Pad\n\nBody of the skill.\n"

func claudeTool() AgentTool { return *ResolveTool("claude") }

func TestDecideSkillWrite(t *testing.T) {
	tool := claudeTool()
	expected := FormatForTool(tool, []byte(testSkill))
	older := []byte(strings.Replace(testSkill, "Body of the skill.", "An older body.", 1))
	stamp := func(b []byte, v string) []byte { return StampSkill(FormatForTool(tool, b), v) }
	crlf := func(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), "\n", "\r\n")) }

	cases := []struct {
		name     string
		existing []byte
		exists   bool
		running  string
		want     SkillAction
	}{
		{"missing", nil, false, "v0.18.0", SkillInstall},
		{"same text, stamped", stamp([]byte(testSkill), "v0.18.0"), true, "v0.18.0", SkillUnchanged},
		{"older pad's unedited text", stamp(older, "v0.17.0"), true, "v0.18.0", SkillUpdate},
		{"edited after stamping", append(stamp(older, "v0.17.0"), []byte("my line\n")...), true, "v0.18.0", SkillKeepEdited},
		{"edited body, stamp left", []byte(strings.Replace(string(stamp(older, "v0.17.0")), "older", "edited", 1)), true, "v0.18.0", SkillKeepEdited},
		{"newer pad's text", stamp(older, "v0.19.0"), true, "v0.18.0", SkillKeepNewer},
		{"newer rc than this release", stamp(older, "v0.18.0-rc.9"), true, "v0.18.0-rc.7", SkillKeepNewer},
		{"dev build wrote it", stamp(older, "dev"), true, "v0.18.0", SkillUpdate},
		{"running a dev build", stamp(older, "v0.19.0"), true, "dev", SkillUpdate},
		{"dev build, but edited", append(stamp(older, "v0.19.0"), []byte("x\n")...), true, "dev", SkillKeepEdited},
		// The lead's condition: line endings never read as an edit.
		{"CRLF copy of an unedited older file", crlf(stamp(older, "v0.17.0")), true, "v0.18.0", SkillUpdate},
		{"extra final newline on an unedited older file", append(stamp(older, "v0.17.0"), '\n', '\n'), true, "v0.18.0", SkillUpdate},
		{"CRLF copy of the current file", crlf(stamp([]byte(testSkill), "v0.18.0")), true, "v0.18.0", SkillUnchanged},
		{"unstamped, already this text", expected, true, "v0.18.0", SkillUpdate},
		{"unstamped, matches no release", []byte("hand-written skill\n"), true, "v0.18.0", SkillKeepEdited},
	}
	for _, c := range cases {
		got := DecideSkillWrite(tool, c.existing, c.exists, expected, c.running)
		if got.Action != c.want {
			t.Errorf("%s: action %d, want %d", c.name, got.Action, c.want)
		}
	}
}

// An unstamped copy a past release wrote is recognised from the frozen list,
// with any line endings, and updates quietly; the same bytes edited do not.
func TestDecideSkillWriteLegacyCopies(t *testing.T) {
	head, err := os.ReadFile("../../skills/pad/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range SupportedTools {
		legacy := FormatForTool(tool, head) // what this tree wrote, unstamped
		if !legacySkillHashKnown(tool, skillHash(legacy)) {
			t.Fatalf("%s: the current unstamped output is not in the frozen list; regenerate it", tool.Name)
		}
		other := []byte("a newer skill text\n")
		for name, b := range map[string][]byte{
			"as written": legacy,
			"CRLF":       []byte(strings.ReplaceAll(string(legacy), "\n", "\r\n")),
			"final \\n":  append(append([]byte{}, legacy...), '\n'),
		} {
			if got := DecideSkillWrite(tool, b, true, other, "v0.18.0").Action; got != SkillUpdate {
				t.Errorf("%s %s: action %d, want update", tool.Name, name, got)
			}
		}
		edited := append(append([]byte{}, legacy...), []byte("\nour own rule\n")...)
		if got := DecideSkillWrite(tool, edited, true, other, "v0.18.0").Action; got != SkillKeepEdited {
			t.Errorf("%s edited legacy copy: action %d, want keep", tool.Name, got)
		}
	}
}

// WriteSkill keeps what it decides to keep unless forced, and what it writes
// carries a stamp that reads back as unedited.
func TestWriteSkill(t *testing.T) {
	dir := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	tool := claudeTool()
	path := filepath.Join(dir, tool.SkillDir, tool.SkillFile)

	res, err := WriteSkill(tool, []byte(testSkill), "v0.18.0", false)
	if err != nil || res.Action != SkillInstall || !res.Wrote {
		t.Fatalf("install: %+v %v", res, err)
	}
	b, _ := os.ReadFile(path)
	if _, _, _, ok := parseSkillStamp(b); !ok {
		t.Fatalf("written file carries no stamp:\n%s", b)
	}
	if res, _ := WriteSkill(tool, []byte(testSkill), "v0.18.0", false); res.Action != SkillUnchanged || res.Wrote {
		t.Fatalf("second write: %+v", res)
	}

	mine := []byte("my own skill\n")
	if err := os.WriteFile(path, mine, 0o644); err != nil {
		t.Fatal(err)
	}
	if res, _ := WriteSkill(tool, []byte(testSkill), "v0.18.0", false); res.Action != SkillKeepEdited || res.Wrote {
		t.Fatalf("edited file: %+v", res)
	}
	if b, _ := os.ReadFile(path); string(b) != string(mine) {
		t.Fatal("an edited file was rewritten without force")
	}
	if res, _ := WriteSkill(tool, []byte(testSkill), "v0.18.0", true); !res.Wrote {
		t.Fatalf("force did not write: %+v", res)
	}
}

func TestSkillVersionNewer(t *testing.T) {
	for _, c := range []struct {
		stamp, running string
		want           bool
	}{
		{"v0.19.0", "v0.18.0", true},
		{"v0.18.0", "v0.18.0", false},
		{"v0.17.9", "v0.18.0", false},
		{"v0.18.0", "v0.18.0-rc.7", true},
		{"v0.18.0-rc.7", "v0.18.0", false},
		{"v0.18.0-rc.10", "v0.18.0-rc.9", true},
		{"v0.18.0-rc.9", "v0.18.0-rc.10", false},
		{"v1.0.0", "v0.99.99", true},
		{"0.19.0", "v0.18.0", true},
		{"v0.19.0+build.5", "v0.18.0", true},
		{"dev", "v0.18.0", false},
		{"v0.19.0", "dev", false},
		{"unknown", "v0.18.0", false},
		{"v0.19", "v0.18.0", false},
		// Codex r1: SemVer numbers have no size limit.
		{"v99999999999999999999.0.0", "v1.0.0", true},
		{"v1.0.0", "v99999999999999999999.0.0", false},
		{"v1.0.0-rc.99999999999999999999", "v1.0.0-rc.2", true},
		{"v1.0.0-rc.2", "v1.0.0-rc.99999999999999999999", false},
		{"v1.0.0-rc.99999999999999999999", "v1.0.0-rc.alpha", false},
	} {
		if got := skillVersionNewer(c.stamp, c.running); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.stamp, c.running, got, c.want)
		}
	}
}
