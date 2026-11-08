package buildtools

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
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

	// Triggers is the set of keys under on:, read separately because a bare
	// key such as workflow_dispatch: decodes to a nil pointer, which a struct
	// cannot tell from an absent one. Push and PullRequest hold on:'s push
	// and pull_request mappings whole.
	Triggers    []string       `yaml:"-"`
	Push        map[string]any `yaml:"-"`
	PullRequest map[string]any `yaml:"-"`
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
	var raw struct {
		On map[string]any `yaml:"on"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	wf.Triggers = slices.Sorted(maps.Keys(raw.On))
	wf.Push, _ = raw.On["push"].(map[string]any)
	wf.PullRequest, _ = raw.On["pull_request"].(map[string]any)
	return wf
}

// fold collapses whitespace, so a folded (>-) expression compares as one line.
func fold(expr string) string { return strings.Join(strings.Fields(expr), " ") }

// conjuncts splits a GitHub expression on && after folding whitespace, and
// refuses one that has a || anywhere: an OR can widen any guard it touches.
func conjuncts(t *testing.T, where, expr string) []string {
	t.Helper()
	expr = fold(expr)
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
	// main SHA neither queues behind nor replaces main's run of that SHA. Pinned
	// whole: in ci.yml the PR alternative names github.ref on its own, so a
	// substring check would pass a push group keyed on the SHA alone.
	groups := map[string]string{
		"ci.yml":   "ci-${{ github.event_name == 'push' && format('{0}-{1}', github.ref, github.sha) || github.ref }}",
		"race.yml": "race-${{ github.ref }}-${{ github.sha }}",
	}
	triggers := map[string][]string{
		"ci.yml":   {"pull_request", "push"},
		"race.yml": {"push", "workflow_dispatch"},
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
		if !slices.Equal(wf.Triggers, triggers[name]) {
			t.Errorf("%s: triggers = %v, want %v", name, wf.Triggers, triggers[name])
		}
		// branches is push's only filter: a paths, tags or *-ignore filter
		// could skip a release push.
		if keys := slices.Sorted(maps.Keys(wf.Push)); !slices.Equal(keys, []string{"branches"}) {
			t.Errorf("%s: on.push keys = %v, want only [branches]", name, keys)
		}
	}

	// Pull requests run into main and into release-* (TASK-4403), so a
	// backport is gated on the PR and not only after it lands.
	ci := readWorkflow(t, "ci.yml")
	if ci.On.PullRequest == nil {
		t.Fatalf("ci.yml: on.pull_request is missing")
	}
	prBranches := slices.Clone(ci.On.PullRequest.Branches)
	slices.Sort(prBranches)
	if !slices.Equal(prBranches, want) {
		t.Errorf("ci.yml: on.pull_request.branches = %v, want %v", ci.On.PullRequest.Branches, want)
	}
	if keys := slices.Sorted(maps.Keys(ci.PullRequest)); !slices.Equal(keys, []string{"branches"}) {
		t.Errorf("ci.yml: on.pull_request keys = %v, want only [branches]", keys)
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

// The push-only work runs on every push (main and release-*), never on PRs:
// the race steps, their stop-the-line notices (which name the branch rather
// than assuming main), and the traced PostgreSQL ./... run with --expect.
func TestCIWorkflow_PushOnlyWorkRunsOnEveryPush(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	const stopIf = "always() && github.event_name == 'push' && (steps.race.outcome == 'failure' || steps.race.outcome == 'cancelled')"
	for _, name := range []string{"go", "go-postgres"} {
		job, ok := ci.Jobs[name]
		if !ok {
			t.Fatalf("ci.yml has no %s job", name)
		}
		var race, stop bool
		for _, s := range job.Steps {
			if s.ID == "race" {
				race = true
				if fold(s.If) != "github.event_name == 'push'" {
					t.Errorf("%s race step if = %q, want exactly github.event_name == 'push'", name, s.If)
				}
			}
			if strings.Contains(s.Run, "STOP THE LINE") {
				stop = true
				if fold(s.If) != stopIf {
					t.Errorf("%s stop-the-line if = %q, want exactly %q", name, s.If, stopIf)
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

	var traced bool
	for _, s := range ci.Jobs["go-postgres"].Steps {
		if s.Name != "Run tests against PostgreSQL" {
			continue
		}
		traced = true
		if s.If != "" {
			t.Errorf("go-postgres %q if = %q; it must run on every event", s.Name, s.If)
		}
		// A PR runs the listed packages; every other event, i.e. any push,
		// runs the whole traced suite against that list. Pinned whole, so the
		// branches cannot swap and pipefail cannot go missing unseen.
		if fold(s.Run) != fold(pgRun) {
			t.Errorf("go-postgres %q run = %q, want %q", s.Name, s.Run, pgRun)
		}
	}
	if !traced {
		t.Error("ci.yml go-postgres has no \"Run tests against PostgreSQL\" step")
	}

	race := readWorkflow(t, "race.yml")
	if jobs := slices.Sorted(maps.Keys(race.Jobs)); !slices.Equal(jobs, []string{"go-server-race", "go-server-race-postgres"}) {
		t.Errorf("race.yml jobs = %v, want [go-server-race go-server-race-postgres]", jobs)
	}
	for name, job := range race.Jobs {
		if job.If != "" {
			t.Errorf("race.yml %s if = %q; Race jobs run on every trigger", name, job.If)
		}
		var stop bool
		for _, s := range job.Steps {
			if strings.Contains(s.Run, "STOP THE LINE") {
				stop = true
				if fold(s.If) != "failure() || cancelled()" {
					t.Errorf("race.yml %s stop-the-line if = %q, want exactly failure() || cancelled()", name, s.If)
				}
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

// pgRun is go-postgres's "Run tests against PostgreSQL" step, whole.
const pgRun = `set -euo pipefail
if [ "$GITHUB_EVENT_NAME" = pull_request ]; then
  mapfile -t packages < <(sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' scripts/ci-pg-packages.txt)
  test "${#packages[@]}" -gt 0
  go test -timeout=45m -count=1 "${packages[@]}"
else
  PAD_TEST_PG_TRACE=1 go test -json -timeout=45m -count=1 ./... \
    | tee /tmp/pg-full.jsonl \
    | python3 scripts/ci-pg-trace.py --expect scripts/ci-pg-packages.txt --show-output
fi
`

// The exact strings allowed to name the branch, and where. Everything else
// that names it is a gate. Exact strings rather than names or a blanket
// strip: a copied guard, or a new use of $GITHUB_REF_NAME, still fails.
const (
	healIf    = "github.event_name == 'push' && github.ref == 'refs/heads/main' && needs.nix.outputs.vendor_hash_bumped == 'true'"
	refuseIf  = "steps.vendorhash.outputs.bumped == 'true' && github.event_name == 'push' && github.ref != 'refs/heads/main'"
	refuseMsg = `echo "::error::nix/package.nix has a stale vendorHash at $GITHUB_SHA on $GITHUB_REF_NAME, and only main is healed. Bring main's healed vendorHash into this branch and push again."`
	stopLine  = `echo "# STOP THE LINE: a race run on $GITHUB_REF_NAME failed or did not finish"`
)

// branchMarks are the ways a workflow can read the branch, matched without
// regard to case (GitHub's expression contexts are case-insensitive): the
// github context's and event payload's ref fields (github.ref also covers
// ref_name), the runner's GITHUB_REF* variables and event file, and git
// asking which branch is checked out.
var branchMarks = []string{
	"github.ref", "head_ref", "base_ref", "github.event.ref", "['ref']", `["ref"]`,
	"default_branch", "github_ref", "github_head_ref", "github_base_ref", "github_event_path",
	"refs/heads", "abbrev-ref", "show-current", "symbolic-ref", "symbolic-full-name",
	"name-rev", "for-each-ref", "origin/main", "origin/head",
}

// wholeGithub matches the bare github context handed to a function or
// indexed (toJSON(github), format('{0}', github), github['ref']), which can
// reach the ref without naming it.
var wholeGithub = regexp.MustCompile(`(?i)\bgithub\s*[\[\),]`)

// branchKeys are keys whose value pins a branch as data, such as a checkout
// step's with: ref: main. on: is pinned separately and not scanned.
var branchKeys = []string{"ref", "branch", "branches"}

// scalars collects every string in a decoded YAML tree, keys included, so a
// scan of it covers every field (if, run, env, with, continue-on-error,
// strategy, runs-on, needs, outputs, ...) without naming them. Comments are
// not part of the tree. A key is prefixed "key:" so branchKeys can tell a
// key from a value.
func scalars(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			*out = append(*out, "key:"+k)
			scalars(vv, out)
		}
	case []any:
		for _, vv := range x {
			scalars(vv, out)
		}
	case nil:
	default:
		*out = append(*out, fmt.Sprint(x))
	}
}

// branchHits returns the scalars that name the branch, after dropping the
// allowed whole lines (compared exactly, before case folding).
func branchHits(v any, allowLines []string) []string {
	var all, hits []string
	scalars(v, &all)
	for _, sc := range all {
		if key, ok := strings.CutPrefix(sc, "key:"); ok {
			if slices.Contains(branchKeys, strings.ToLower(key)) {
				hits = append(hits, sc)
			}
			sc = key
		}
		var kept []string
		for _, line := range strings.Split(sc, "\n") {
			if !slices.Contains(allowLines, strings.TrimSpace(line)) {
				kept = append(kept, line)
			}
		}
		rest := strings.ToLower(strings.Join(kept, "\n"))
		if wholeGithub.MatchString(rest) {
			hits = append(hits, sc)
			continue
		}
		for _, mark := range branchMarks {
			if strings.Contains(rest, mark) {
				hits = append(hits, sc)
				break
			}
		}
	}
	return hits
}

// readRawWorkflow decodes a workflow as a plain tree, and refuses a file
// with more than one YAML document, which a single decode would not see.
func readRawWorkflow(t *testing.T, file string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", file))
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("%s: more than one YAML document (or a trailing decode error: %v)", file, err)
	}
	return raw
}

// takeIf removes a mapping's if when it equals want (folded), and reports
// whether it did. An allowance is for that key's exact value only.
func takeIf(m map[string]any, want string) (map[string]any, bool) {
	cond, ok := m["if"].(string)
	if !ok || fold(cond) != want {
		return m, false
	}
	m = maps.Clone(m)
	delete(m, "if")
	return m, true
}

// Nothing else in either workflow may depend on the branch: a job or step
// gated to main would make a release-* push run less than a main push does.
// The deliberate exceptions, each an exact value in a fixed place: the heal
// job's if; the Nix refusal step's if and its message line, in the nix job,
// on exactly one step; and the stop-the-line notice line. on: and
// concurrency: are pinned exactly elsewhere. Local actions and reusable
// workflows run code this scan cannot read, so neither may appear.
func TestCIWorkflows_NoOtherBranchGates(t *testing.T) {
	for _, file := range []string{"ci.yml", "race.yml"} {
		raw := readRawWorkflow(t, file)
		for key, v := range raw {
			if key == "on" || key == "concurrency" || key == "jobs" {
				continue
			}
			if hits := branchHits(v, nil); len(hits) > 0 {
				t.Errorf("%s: top-level %s depends on the branch: %q", file, key, hits)
			}
		}
		jobs, ok := raw["jobs"].(map[string]any)
		if !ok || len(jobs) == 0 {
			t.Fatalf("%s: no jobs", file)
		}
		refusals := 0
		for name, jv := range jobs {
			job, ok := jv.(map[string]any)
			if !ok {
				t.Fatalf("%s %s: job is not a mapping", file, name)
			}
			fields := maps.Clone(job)
			delete(fields, "steps")
			if file == "ci.yml" && name == "push-vendor-hash" {
				var took bool
				if fields, took = takeIf(fields, healIf); !took {
					t.Errorf("ci.yml push-vendor-hash: if = %q, want exactly %q", job["if"], healIf)
				}
			}
			if hits := branchHits(fields, nil); len(hits) > 0 {
				t.Errorf("%s %s: job depends on the branch: %q", file, name, hits)
			}
			if uses, ok := job["uses"].(string); ok && strings.HasPrefix(uses, "./") {
				t.Errorf("%s %s: uses a local reusable workflow %q, which this scan cannot read", file, name, uses)
			}
			steps, _ := job["steps"].([]any)
			for i, sv := range steps {
				step, ok := sv.(map[string]any)
				if !ok {
					t.Fatalf("%s %s step %d: not a mapping", file, name, i)
				}
				lineAllow := []string{stopLine}
				if file == "ci.yml" && name == "nix" {
					var took bool
					if step, took = takeIf(step, refuseIf); took {
						refusals++
						lineAllow = append(lineAllow, refuseMsg)
					}
				}
				if hits := branchHits(step, lineAllow); len(hits) > 0 {
					t.Errorf("%s %s step %d depends on the branch: %q", file, name, i, hits)
				}
				if uses, ok := step["uses"].(string); ok && strings.HasPrefix(uses, "./") {
					t.Errorf("%s %s step %d: uses a local action %q, which this scan cannot read", file, name, i, uses)
				}
			}
		}
		if file == "ci.yml" && refusals != 1 {
			t.Errorf("ci.yml nix: %d steps carry the stale-hash refusal's if, want exactly 1", refusals)
		}
	}
}
