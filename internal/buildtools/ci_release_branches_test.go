package buildtools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

// TASK-4253: ci.yml and race.yml run on pushes to release-* as well as main,
// so the exact SHA a release is tagged at carries CI and Race runs of its own.
// These tests pin the branch-dependent behaviour that change has to keep: what
// runs where, and the one job that must never run off main.

type workflowStep struct {
	ID              string `yaml:"id"`
	Name            string `yaml:"name"`
	If              string `yaml:"if"`
	Run             string `yaml:"run"`
	ContinueOnError any    `yaml:"continue-on-error"`
}

type workflowJob struct {
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowFile struct {
	On struct {
		Push struct {
			Branches []string `yaml:"branches"`
		} `yaml:"push"`
		PullRequest *struct {
			Branches []string `yaml:"branches"`
		} `yaml:"pull_request"`
	} `yaml:"on"`
	Concurrency struct {
		Group string `yaml:"group"`
	} `yaml:"concurrency"`
	Jobs map[string]workflowJob `yaml:"jobs"`
}

func readWorkflow(t *testing.T, name string) workflowFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return wf
}

// conjuncts splits a GitHub expression on && after folding whitespace, and
// refuses one that has a || anywhere: an OR can widen any guard it touches.
func conjuncts(t *testing.T, where, expr string) []string {
	t.Helper()
	expr = strings.Join(strings.Fields(expr), " ")
	if strings.Contains(expr, "||") {
		t.Fatalf("%s: if = %q; it must be a plain && of terms, with no ||", where, expr)
	}
	var terms []string
	for _, term := range strings.Split(expr, "&&") {
		terms = append(terms, strings.TrimSpace(term))
	}
	return terms
}

func TestCIWorkflows_RunOnMainAndReleasePushes(t *testing.T) {
	want := []string{"main", "release-*"}
	// A push's group carries the ref AND the SHA, so a release branch cut at a
	// main SHA neither queues behind nor evicts main's run of that SHA. Pinned
	// whole: in ci.yml the PR alternative names github.ref on its own, so a
	// substring check would pass a push group keyed on the SHA alone.
	groups := map[string]string{
		"ci.yml":   "ci-${{ github.event_name == 'push' && format('{0}-{1}', github.ref, github.sha) || github.ref }}",
		"race.yml": "race-${{ github.ref }}-${{ github.sha }}",
	}
	for name, group := range groups {
		wf := readWorkflow(t, name)
		got := slices.Clone(wf.On.Push.Branches)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: on.push.branches = %v, want %v", name, wf.On.Push.Branches, want)
		}
		if wf.Concurrency.Group != group {
			t.Errorf("%s: concurrency.group = %q, want %q", name, wf.Concurrency.Group, group)
		}
	}

	// Pull requests are unchanged: PRs into main only.
	ci := readWorkflow(t, "ci.yml")
	if ci.On.PullRequest == nil || !slices.Equal(ci.On.PullRequest.Branches, []string{"main"}) {
		t.Errorf("ci.yml: on.pull_request = %+v, want branches [main]", ci.On.PullRequest)
	}
	if race := readWorkflow(t, "race.yml"); race.On.PullRequest != nil {
		t.Errorf("race.yml: on.pull_request = %+v; Race runs on pushes and dispatch only", race.On.PullRequest)
	}
}

// The heal job commits to the branch its run is on. On a release branch that
// would add a commit after the tree being tagged, so it runs on main only.
func TestCIWorkflow_VendorHashHealRunsOnlyOnMainPushes(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	job, ok := ci.Jobs["push-vendor-hash"]
	if !ok {
		t.Fatal("ci.yml has no push-vendor-hash job")
	}
	terms := conjuncts(t, "push-vendor-hash", job.If)
	for _, need := range []string{"github.event_name == 'push'", "github.ref == 'refs/heads/main'"} {
		if !slices.Contains(terms, need) {
			t.Errorf("push-vendor-hash if = %q; it must include the term %q", job.If, need)
		}
	}
	// It is the only job with a write token; nothing else may push.
	for name, j := range ci.Jobs {
		if j.Permissions["contents"] == "write" && name != "push-vendor-hash" {
			t.Errorf("job %s has contents: write; only push-vendor-hash may", name)
		}
	}
}

// Off main nothing heals, so a stale committed vendorHash must fail the Nix
// job on a push there instead of passing on the recomputed tree.
func TestCIWorkflow_NixRefusesStaleVendorHashOffMain(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	job, ok := ci.Jobs["nix"]
	if !ok {
		t.Fatal("ci.yml has no nix job")
	}
	recompute, refuse := -1, -1
	for i, s := range job.Steps {
		if s.ID == "vendorhash" {
			recompute = i
		}
		if !strings.Contains(s.If, "steps.vendorhash.outputs.bumped == 'true'") || !strings.Contains(s.If, "refs/heads/main") {
			continue
		}
		refuse = i
		terms := conjuncts(t, "nix step "+s.Name, s.If)
		for _, need := range []string{
			"steps.vendorhash.outputs.bumped == 'true'",
			"github.event_name == 'push'",
			"github.ref != 'refs/heads/main'",
		} {
			if !slices.Contains(terms, need) {
				t.Errorf("nix step %q if = %q; it must include the term %q", s.Name, s.If, need)
			}
		}
		if !strings.Contains(s.Run, "exit 1") {
			t.Errorf("nix step %q must fail the job (exit 1)", s.Name)
		}
		if s.ContinueOnError != nil {
			t.Errorf("nix step %q sets continue-on-error; the refusal must fail the job", s.Name)
		}
	}
	if recompute < 0 || refuse < 0 || refuse < recompute {
		t.Fatalf("nix job: recompute step at %d, stale-hash refusal at %d; the refusal must exist and follow the recompute", recompute, refuse)
	}
}

// The race steps run on every push (main and release-*), never on PRs, and
// their stop-the-line notice names the branch rather than assuming main.
func TestCIWorkflow_RaceRunsOnEveryPush(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	for _, name := range []string{"go", "go-postgres"} {
		job, ok := ci.Jobs[name]
		if !ok {
			t.Fatalf("ci.yml has no %s job", name)
		}
		var race, stop bool
		for _, s := range job.Steps {
			if s.ID == "race" {
				race = true
				if strings.Join(strings.Fields(s.If), " ") != "github.event_name == 'push'" {
					t.Errorf("%s race step if = %q, want exactly github.event_name == 'push'", name, s.If)
				}
			}
			if strings.Contains(s.Run, "STOP THE LINE") {
				stop = true
				if strings.Contains(s.If, "refs/heads/") {
					t.Errorf("%s stop-the-line if = %q; it must fire on any push", name, s.If)
				}
				if !strings.Contains(s.Run, "$GITHUB_REF_NAME") {
					t.Errorf("%s stop-the-line notice must name the branch ($GITHUB_REF_NAME)", name)
				}
			}
		}
		if !race || !stop {
			t.Errorf("%s: race step present %v, stop-the-line step present %v", name, race, stop)
		}
	}
	race := readWorkflow(t, "race.yml")
	for name, job := range race.Jobs {
		var stop bool
		for _, s := range job.Steps {
			if strings.Contains(s.Run, "STOP THE LINE") {
				stop = true
				if !strings.Contains(s.Run, "$GITHUB_REF_NAME") {
					t.Errorf("race.yml %s stop-the-line notice must name the branch ($GITHUB_REF_NAME)", name)
				}
			}
		}
		if !stop {
			t.Errorf("race.yml %s has no stop-the-line step", name)
		}
	}
}
