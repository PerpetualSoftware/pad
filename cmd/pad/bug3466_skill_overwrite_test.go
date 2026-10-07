package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-3466: pad init used to rewrite every installed skill file that differed
// from the running binary's, so a team's edit was lost and an older pad
// rewrote a newer pad's file. These drive the real door, ensureSkills, in a
// temp project with PATH emptied so only Claude Code (always included) is
// installed.

func setupSkillTest(t *testing.T) string {
	t.Helper()
	project := setupEnsureWorkspaceTest(t)
	t.Setenv("PATH", "")
	prev := version
	version = "v0.18.0"
	t.Cleanup(func() { version = prev })
	return project
}

func claudeSkillPath(project string) string {
	return filepath.Join(project, ".claude", "skills", "pad", "SKILL.md")
}

func writeSkillFile(t *testing.T, project, content string) {
	t.Helper()
	p := claudeSkillPath(project)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readSkill(t *testing.T, project string) string {
	t.Helper()
	b, err := os.ReadFile(claudeSkillPath(project))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// testStamp is the marker line, computed independently of the code under test:
// sha256 over the body with CRLF made LF and trailing newlines dropped.
func testStamp(body, ver string) string {
	n := strings.TrimRight(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	sum := sha256.Sum256([]byte(n))
	return "<!-- pad:skill v=" + ver + " sha256=" + hex.EncodeToString(sum[:]) + " -->\n"
}

func TestBUG3466_InitKeepsAnEditedSkill(t *testing.T) {
	project := setupSkillTest(t)
	const mine = "---\nname: pad\n---\n\nOur team's own /pad rules.\n"
	writeSkillFile(t, project, mine)
	ensureSkills()
	if got := readSkill(t, project); got != mine {
		t.Fatalf("pad init overwrote an edited skill:\n%s", got)
	}
}

func TestBUG3466_InitDoesNotDowngradeANewerSkill(t *testing.T) {
	project := setupSkillTest(t)
	body := "---\nname: pad\n---\n\nThe skill a newer pad wrote.\n"
	newer := body + testStamp(body, "v99.0.0")
	writeSkillFile(t, project, newer)
	ensureSkills()
	if got := readSkill(t, project); got != newer {
		t.Fatalf("an older pad rewrote a newer pad's skill:\n%s", got)
	}
}
