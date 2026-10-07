package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
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

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureSkillStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = pw
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	pw.Close()
	os.Stdout = orig
	return <-done
}

// Codex r1: pad workspace init/link skipped every installed skill, so an
// unedited older copy was never updated there. It now asks the same door.
func TestBUG3466_WorkspaceInitUpdatesAnOlderUneditedSkill(t *testing.T) {
	project := setupSkillTest(t)
	withClosedStdin(t)
	body := "---\nname: pad\n---\n\nAn older pad's skill.\n"
	writeSkillFile(t, project, body+testStamp(body, "v0.17.0"))
	captureSkillStdout(t, offerSkillInstall)
	got := readSkill(t, project)
	if strings.Contains(got, "An older pad's skill.") || !strings.Contains(got, "<!-- pad:skill v=v0.18.0 ") {
		t.Fatalf("workspace init left an unedited older skill in place:\n%s", got)
	}

	mine := "---\nname: pad\n---\n\nOur own rules.\n"
	writeSkillFile(t, project, mine)
	captureSkillStdout(t, offerSkillInstall)
	if got := readSkill(t, project); got != mine {
		t.Fatalf("workspace init overwrote an edited skill:\n%s", got)
	}
}

// Codex r1: after keeping an edited file, pad agent update must not then say
// nothing is installed or everything is up to date.
func TestBUG3466_AgentUpdateDoesNotContradictAKeep(t *testing.T) {
	project := setupSkillTest(t)
	writeSkillFile(t, project, "---\nname: pad\n---\n\nOur own rules.\n")
	out := captureSkillStdout(t, func() { _ = installUpdate(false) })
	for _, wrong := range []string{"No tools installed", "All installations are up to date"} {
		if strings.Contains(out, wrong) {
			t.Errorf("agent update kept an edited file and then said %q:\n%s", wrong, out)
		}
	}
	if !strings.Contains(out, "kept") {
		t.Errorf("agent update did not say a file was kept:\n%s", out)
	}
}

// Codex r1: the status listing names --force for a newer pad's file too.
func TestBUG3466_ListNamesForceForANewerSkill(t *testing.T) {
	project := setupSkillTest(t)
	body := "---\nname: pad\n---\n\nThe skill a newer pad wrote.\n"
	writeSkillFile(t, project, body+testStamp(body, "v99.0.0"))
	recordInstallation("claude", claudeSkillPath(project))
	out := captureSkillStdout(t, func() { _ = installList() })
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "newer pad") {
			line = l
		}
	}
	if !strings.Contains(line, "--force") {
		t.Fatalf("the newer-pad status line names no way to replace it:\n%s", out)
	}
}

// Codex r2: the local part of --list asks the same decision, so an edited
// committed skill in a fresh checkout (nothing in the registry) is not shown
// as healthy.
func TestBUG3466_ListFlagsAnUntrackedEditedSkill(t *testing.T) {
	project := setupSkillTest(t)
	writeSkillFile(t, project, "---\nname: pad\n---\n\nOur own rules.\n")
	out := captureSkillStdout(t, func() { _ = installList() })
	if !strings.Contains(out, "edited") || !strings.Contains(out, "--force") {
		t.Fatalf("an untracked edited skill was listed as healthy:\n%s", out)
	}
}

// Codex r2: an untracked skill that is already current is installed, so
// agent update must not say nothing is installed; and it is recorded.
func TestBUG3466_UpdateCountsAnUntrackedCurrentSkill(t *testing.T) {
	_ = setupSkillTest(t)
	if _, err := writeSkill(*cliToolClaude(), false); err != nil {
		t.Fatal(err)
	}
	// Forget the registry entry the install made.
	home, _ := os.UserHomeDir()
	_ = os.Remove(filepath.Join(home, ".pad", "installations.json"))
	out := captureSkillStdout(t, func() { _ = installUpdate(false) })
	if strings.Contains(out, "No tools installed") {
		t.Fatalf("agent update said nothing is installed beside a current skill:\n%s", out)
	}
	reg, err := loadRegistryForTest()
	if err != nil || len(reg) == 0 {
		t.Fatalf("a current skill was not recorded by agent update: %v %v", reg, err)
	}
}

func cliToolClaude() *cli.AgentTool { return cli.ResolveTool("claude") }

func loadRegistryForTest() ([]cli.Installation, error) {
	reg, err := cli.LoadRegistry()
	if err != nil {
		return nil, err
	}
	return reg.Installations, nil
}
