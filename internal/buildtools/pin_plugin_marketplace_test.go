package buildtools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

// TASK-3487: scripts/pin-plugin-marketplace.sh points the
// Claude Code plugin marketplace at a stable release tag, and the release
// workflow runs it after a stable release without ever failing the run.

const marketplaceFixture = `{
  "name": "pad",
  "owner": {
    "name": "Perpetual Software LLC",
    "url": "https://getpad.dev"
  },
  "plugins": [
    {
      "name": "pad",
      "source": "./plugin",
      "description": "Talk to your project — a description with a non-ASCII dash."
    }
  ],
  "description": "Official Pad marketplace."
}
`

// pinRepo is a throwaway git repo with the real script, a marketplace.json,
// and tags whose plugin.json does or does not pin a version.
func pinRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "pin-plugin-marketplace.sh"))
	if err != nil {
		t.Fatal(err)
	}
	run("init", "-q", "-b", "main")
	write("scripts/pin-plugin-marketplace.sh", string(script))
	if err := os.Chmod(filepath.Join(dir, "scripts/pin-plugin-marketplace.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(".claude-plugin/marketplace.json", marketplaceFixture)
	write("plugin/.claude-plugin/plugin.json", `{"name": "pad", "version": "0.3.3"}`+"\n")
	run("add", "-A")
	run("commit", "-q", "-m", "pinned")
	run("tag", "v0.17.2")
	write("plugin/.claude-plugin/plugin.json", `{"name": "pad"}`+"\n")
	run("commit", "-q", "-am", "unpinned")
	run("tag", "v0.18.0-rc.1")
	run("tag", "v0.18.0")
	run("tag", "v0.18.1")
	return dir
}

func runPin(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"scripts/pin-plugin-marketplace.sh"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func marketplaceSource(t *testing.T, dir string) any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".claude-plugin/marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Plugins[0]["source"]
}

func TestPinPluginMarketplace_RewritesOnlyTheSource(t *testing.T) {
	dir := pinRepo(t)
	out, err := runPin(t, dir, "v0.18.0")
	if err != nil {
		t.Fatalf("pin v0.18.0: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(dir, ".claude-plugin/marketplace.json"))
	want := strings.Replace(marketplaceFixture, `"source": "./plugin",`, `"source": {
        "source": "git-subdir",
        "url": "https://github.com/PerpetualSoftware/pad.git",
        "path": "plugin",
        "ref": "v0.18.0"
      },`, 1)
	if string(got) != want {
		t.Fatalf("marketplace.json after the pin:\n%s\nwant:\n%s", got, want)
	}

	// Again for the same tag: nothing to do.
	if out, err := runPin(t, dir, "v0.18.0"); err != nil || !strings.Contains(out, "already pinned") {
		t.Fatalf("re-pin: %v %s", err, out)
	}
	// A later stable tag moves the ref.
	if out, err := runPin(t, dir, "v0.18.1"); err != nil {
		t.Fatalf("pin v0.18.1: %v\n%s", err, out)
	}
	if src, _ := marketplaceSource(t, dir).(map[string]any); src["ref"] != "v0.18.1" {
		t.Fatalf("ref after the second pin = %v", src["ref"])
	}
}

func TestPinPluginMarketplace_Refusals(t *testing.T) {
	dir := pinRepo(t)
	for _, c := range []struct{ tag, why string }{
		{"v0.18.0-rc.1", "not a stable release tag"},
		{"main", "not a stable release tag"},
		{"v0.17.2", "pins a version"},
		{"v9.9.9", "not found"},
	} {
		out, err := runPin(t, dir, c.tag)
		if err == nil || !strings.Contains(out, c.why) {
			t.Errorf("pin %s: err=%v out=%q, want a refusal saying %q", c.tag, err, out, c.why)
		}
		if src := marketplaceSource(t, dir); src != "./plugin" {
			t.Errorf("pin %s changed the file to %v", c.tag, src)
		}
	}
}

// The release job runs the pin after the release and never fails the run.
func TestReleaseWorkflow_PinsThePluginWithoutFailingTheRelease(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Needs       any               `yaml:"needs"`
			If          string            `yaml:"if"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				ID              string `yaml:"id"`
				If              string `yaml:"if"`
				Run             string `yaml:"run"`
				ContinueOnError bool   `yaml:"continue-on-error"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	job, ok := wf.Jobs["pin-plugin"]
	if !ok {
		t.Fatal("release.yml has no pin-plugin job")
	}
	if job.Needs != "release" {
		t.Errorf("pin-plugin needs %v, want release (it runs after the release is published)", job.Needs)
	}
	if !strings.Contains(job.If, "contains(github.ref_name, '-')") || !strings.Contains(job.If, "!") {
		t.Errorf("pin-plugin if = %q; it must skip prerelease tags", job.If)
	}
	if job.Permissions["contents"] != "write" || job.Permissions["issues"] != "write" {
		t.Errorf("pin-plugin permissions = %v; it pushes and opens an issue", job.Permissions)
	}
	var pin, report bool
	for _, s := range job.Steps {
		if strings.HasPrefix(strings.TrimSpace(s.Run), "scripts/pin-plugin-marketplace.sh --push") {
			pin = true
			if !s.ContinueOnError || s.ID == "" {
				t.Errorf("the pin step must be continue-on-error with an id, so a failed pin cannot fail the release")
			}
		}
		if strings.Contains(s.If, "outcome == 'failure'") && strings.Contains(s.Run, "gh issue create") {
			report = true
		}
	}
	if !pin || !report {
		t.Errorf("pin step present %v, failure report present %v", pin, report)
	}
}
