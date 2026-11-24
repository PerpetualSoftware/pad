package buildtools

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
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
	ID              string            `yaml:"id"`
	Name            string            `yaml:"name"`
	If              string            `yaml:"if"`
	Env             map[string]string `yaml:"env"`
	Run             string            `yaml:"run"`
	ContinueOnError any               `yaml:"continue-on-error"`
}

type workflowStrategy struct {
	FailFast *bool          `yaml:"fail-fast"`
	Matrix   map[string]any `yaml:"matrix"`
}

type workflowJob struct {
	Name        string            `yaml:"name"`
	If          string            `yaml:"if"`
	Strategy    *workflowStrategy `yaml:"strategy"`
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
// job on a push there, and on a PR into release-*, instead of passing on the
// recomputed tree.
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
		// Pinned whole: a push off main, or a pull request whose base is a
		// release-* branch (TASK-4403). A PR into main stays judged on the
		// recomputed tree, because main is healed after its run.
		if fold(s.If) != refuseIf {
			t.Errorf("nix step %q if = %q, want exactly %q", s.Name, s.If, refuseIf)
		}
		// The script whole, so the event test, the message each event gets and
		// the exit that fails the job cannot drift apart unseen.
		wantRun := "if [ \"$GITHUB_EVENT_NAME\" = pull_request ]; then\n" + refusePRMsg + "\nelse\n" + refuseMsg + "\nfi\nexit 1\n"
		if fold(s.Run) != fold(wantRun) {
			t.Errorf("nix step %q run = %q, want %q", s.Name, s.Run, wantRun)
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
	if jobs := slices.Sorted(maps.Keys(race.Jobs)); !slices.Equal(jobs, slices.Sorted(maps.Keys(raceShardJobs))) {
		t.Errorf("race.yml jobs = %v, want %v", jobs, slices.Sorted(maps.Keys(raceShardJobs)))
	}
	for name, job := range race.Jobs {
		if job.If != "" {
			t.Errorf("race.yml %s if = %q; Race jobs run on every trigger", name, job.If)
		}
		// Each job runs one shard of its package, chosen by the same script
		// and build flags the coverage check in ci.yml proves.
		if pkg, ok := raceShardJobs[name]; ok {
			want := `run=$(scripts/test-shard.sh regex ./` + pkg + `/ 3 "$SHARD" $GO_RACE_FLAGS)`
			var shard bool
			for _, s := range job.Steps {
				if !strings.Contains(s.Run, "test-shard.sh") {
					continue
				}
				shard = true
				lines := strings.Split(strings.TrimSpace(s.Run), "\n")
				if len(lines) != 2 || strings.TrimSpace(lines[0]) != want {
					t.Errorf("race.yml %s shard step run = %q; it must be two lines, the first exactly %q", name, s.Run, want)
					continue
				}
				test := strings.Fields(lines[1])
				if len(test) < 2 || test[0] != "go" || test[1] != "test" ||
					!slices.Contains(test, "$GO_RACE_FLAGS") || !slices.Contains(test, "-timeout=45m") ||
					!strings.HasSuffix(fold(lines[1]), `-run "$run" ./`+pkg+"/") {
					t.Errorf("race.yml %s shard step test line = %q; it must be go test $GO_RACE_FLAGS -timeout=45m ... -run \"$run\" ./%s/", name, lines[1], pkg)
				}
			}
			if !shard {
				t.Errorf("race.yml %s has no test-shard.sh step", name)
			}
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

// raceShardJobs are race.yml's jobs, each with the package it shards. ci.yml's
// race steps exclude exactly these packages (TASK-4141, TASK-4648).
var raceShardJobs = map[string]string{
	"go-server-race":          "internal/server",
	"go-server-race-postgres": "internal/server",
	"go-store-race":           "internal/store",
	"go-store-race-postgres":  "internal/store",
}

// shardChecks are ci.yml's shard coverage jobs, each with its package.
var shardChecks = map[string]string{
	"go-server-shards": "internal/server",
	"go-store-shards":  "internal/store",
}

// raceExclusion is every line of ci.yml's two race steps but the last (the go
// test line), whole.
const raceExclusion = `all=$(go list ./...)
pkgs=$(printf '%s\n' "$all" | grep -Ev '^github.com/PerpetualSoftware/pad/internal/(server|store)$')
test -n "$pkgs"
removed=$(LC_ALL=C comm -23 <(printf '%s\n' "$all" | LC_ALL=C sort) <(printf '%s\n' "$pkgs" | LC_ALL=C sort))
want=$(printf '%s\n' github.com/PerpetualSoftware/pad/internal/server github.com/PerpetualSoftware/pad/internal/store)
[ "$removed" = "$want" ] || { printf '::error::the race step must exclude exactly internal/server and internal/store; it excluded:\n%s\n' "${removed:-(nothing)}"; exit 1; }`

// The ./... race steps leave out exactly the packages race.yml shards, and no
// more: internal/server/render and internal/store/storetest stay in them. The
// exclusion is pinned whole, then run against this tree's real package list,
// so an over-broad pattern or a moved package fails here before CI does.
func TestCIWorkflow_RaceStepsExcludeExactlyTheShardedPackages(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	wantTest := map[string]string{
		"go":          "go test $GO_RACE_FLAGS -timeout=45m -count=1 $pkgs",
		"go-postgres": "go test $GO_RACE_FLAGS -timeout=45m -count=1 $pkgs",
	}
	for name, test := range wantTest {
		var run string
		for _, s := range ci.Jobs[name].Steps {
			if s.ID == "race" {
				run = s.Run
			}
		}
		if run == "" {
			t.Errorf("ci.yml %s has no race step", name)
			continue
		}
		if fold(run) != fold(raceExclusion+"\n"+test) {
			t.Errorf("ci.yml %s race step run = %q, want %q", name, run, raceExclusion+"\n"+test)
		}
	}

	sharded := map[string]bool{}
	for _, pkg := range raceShardJobs {
		sharded[pkg] = true
	}
	if got := slices.Sorted(maps.Keys(sharded)); !slices.Equal(got, []string{"internal/server", "internal/store"}) {
		t.Errorf("race.yml shards %v; the race step exclusion names internal/server and internal/store", got)
	}

	// go list -e: CI creates the web embed before this step, and a checkout
	// that has not built it would otherwise fail to list the root package.
	script := strings.Replace(raceExclusion, "go list ./...", "go list -e ./...", 1)
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script+"\nprintf '%s\\n' $pkgs")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the race step exclusion failed on this tree: %v\n%s", err, out)
	}
	kept := strings.Fields(string(out))
	const mod = "github.com/PerpetualSoftware/pad/"
	for pkg := range sharded {
		if slices.Contains(kept, mod+pkg) {
			t.Errorf("race step keeps %s; race.yml shards it", pkg)
		}
	}
	for _, pkg := range []string{"internal/server/render", "internal/store/storetest", "cmd/pad"} {
		if !slices.Contains(kept, mod+pkg) {
			t.Errorf("race step drops %s; only internal/server and internal/store may go", pkg)
		}
	}
}

// Every sharded package has a coverage check in ci.yml that proves its 3
// shards run each test exactly once, with the shards' build flags, and CI
// result requires it: without one a shard bug drops tests silently.
func TestCIWorkflow_EveryShardedPackageHasARequiredCoverageCheck(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	for job, pkg := range shardChecks {
		j, ok := ci.Jobs[job]
		if !ok {
			t.Errorf("ci.yml has no %s job", job)
			continue
		}
		if j.If != "needs.changes.outputs.go == 'true'" {
			t.Errorf("ci.yml %s if = %q, want needs.changes.outputs.go == 'true'", job, j.If)
		}
		want := "scripts/test-shard.sh check ./" + pkg + "/ 3 $GO_RACE_FLAGS"
		var found bool
		for _, s := range j.Steps {
			if strings.TrimSpace(s.Run) == want {
				found = true
			}
		}
		if !found {
			t.Errorf("ci.yml %s has no step running exactly %q", job, want)
		}
	}
	for _, pkg := range raceShardJobs {
		if !slices.Contains(slices.Collect(maps.Values(shardChecks)), pkg) {
			t.Errorf("race.yml shards %s but ci.yml has no coverage check for it", pkg)
		}
	}

	raw := readRawWorkflow(t, "ci.yml")
	jobs, _ := raw["jobs"].(map[string]any)
	result, _ := jobs["ci-result"].(map[string]any)
	list, _ := result["needs"].([]any)
	var needs []string
	for _, n := range list {
		needs = append(needs, fmt.Sprint(n))
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "ci-result.py"))
	if err != nil {
		t.Fatal(err)
	}
	for job := range shardChecks {
		if !slices.Contains(needs, job) {
			t.Errorf("ci.yml ci-result needs = %v; it must include %s", needs, job)
		}
		if !strings.Contains(string(script), "'"+job+"': 'go'") {
			t.Errorf("scripts/ci-result.py must list %s as a go-scoped required job", job)
		}
	}
}

// pgRun is go-postgres's "Run tests against PostgreSQL" step, whole.
const pgRun = `set -euo pipefail
mapfile -t listed < <(sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' scripts/ci-pg-packages.txt)
test "${#listed[@]}" -gt 0
case "$GITHUB_EVENT_NAME:$PG_LEG" in
  pull_request:store|pull_request:rest)
    printf '%s\n' "${listed[@]}" | grep -qx './internal/store' || { echo "::error::scripts/ci-pg-packages.txt must list ./internal/store" >&2; exit 1; }
    packages=()
    for p in "${listed[@]}"; do
      if [ "$PG_LEG" = store ]; then [ "$p" = ./internal/store ] && packages+=("$p"); else [ "$p" = ./internal/store ] || packages+=("$p"); fi
    done
    test "${#packages[@]}" -gt 0
    go test -timeout=45m -count=1 "${packages[@]}"
    ;;
  pull_request:*)
    echo "::error::unknown PostgreSQL leg on a pull request: $PG_LEG" >&2; exit 1 ;;
  *:full)
    PAD_TEST_PG_TRACE=1 go test -json -timeout=45m -count=1 ./... \
      | tee /tmp/pg-full.jsonl \
      | python3 scripts/ci-pg-trace.py --expect scripts/ci-pg-packages.txt --show-output
    ;;
  *)
    echo "::error::leg $PG_LEG does not run on $GITHUB_EVENT_NAME" >&2; exit 1 ;;
esac
`

// TASK-4820: go-postgres splits pull-request runs into two parallel matrix
// legs (store and rest) while pushes run a single full leg, all inside the
// same job id so ci-result keeps reading one needs.go-postgres.result.
func TestCIWorkflow_PostgresLegs(t *testing.T) {
	ci := readWorkflow(t, "ci.yml")
	job, ok := ci.Jobs["go-postgres"]
	if !ok {
		t.Fatal("ci.yml has no go-postgres job")
	}
	if job.Strategy == nil {
		t.Fatal("go-postgres has no strategy")
	}
	if job.Strategy.FailFast == nil || *job.Strategy.FailFast {
		t.Errorf("go-postgres strategy.fail-fast = %v, want false", job.Strategy.FailFast)
	}
	const wantLeg = `${{ fromJSON(github.event_name == 'pull_request' && '["store","rest"]' || '["full"]') }}`
	leg, ok := job.Strategy.Matrix["leg"].(string)
	if !ok || fold(leg) != fold(wantLeg) {
		t.Errorf("go-postgres strategy.matrix.leg = %#v, want %q", job.Strategy.Matrix["leg"], wantLeg)
	}
	// Exactly the plain name: GitHub appends the leg to each run, and a skipped
	// job reports this name, which merge tooling matches byte for byte.
	if job.Name != "Go (PostgreSQL)" {
		t.Errorf("go-postgres name = %q, want exactly %q", job.Name, "Go (PostgreSQL)")
	}

	const roundTripName = "Native backup/restore round trip against PostgreSQL"
	const roundTripIf = "matrix.leg != 'store'"
	var roundTrip, runTests bool
	for _, s := range job.Steps {
		if s.Name == roundTripName {
			roundTrip = true
			if fold(s.If) != roundTripIf {
				t.Errorf("go-postgres %q if = %q, want exactly %q", s.Name, s.If, roundTripIf)
			}
			continue
		}
		if strings.Contains(s.If, "matrix.leg") {
			t.Errorf("go-postgres step %q if = %q; only %q may mention matrix.leg in if", s.Name, s.If, roundTripName)
		}
		if s.Name == "Run tests against PostgreSQL" {
			runTests = true
			if s.Env["PG_LEG"] != "${{ matrix.leg }}" {
				t.Errorf("go-postgres %q env PG_LEG = %q, want %q", s.Name, s.Env["PG_LEG"], "${{ matrix.leg }}")
			}
		}
	}
	if !roundTrip {
		t.Errorf("ci.yml go-postgres has no %q step", roundTripName)
	}
	if !runTests {
		t.Errorf("ci.yml go-postgres has no %q step", "Run tests against PostgreSQL")
	}

	for id := range ci.Jobs {
		if strings.HasPrefix(id, "go-postgres-") {
			t.Errorf("ci.yml defines job %q; PostgreSQL legs must stay inside go-postgres", id)
		}
	}
	raw := readRawWorkflow(t, "ci.yml")
	jobs, _ := raw["jobs"].(map[string]any)
	result, _ := jobs["ci-result"].(map[string]any)
	list, _ := result["needs"].([]any)
	var needs []string
	for _, n := range list {
		needs = append(needs, fmt.Sprint(n))
	}
	if !slices.Contains(needs, "go-postgres") {
		t.Errorf("ci.yml ci-result needs = %v; it must include go-postgres", needs)
	}
}

// The exact strings allowed to name the branch, and where. Everything else
// that names it is a gate. Exact strings rather than names or a blanket
// strip: a copied guard, or a new use of $GITHUB_REF_NAME, still fails.
const (
	healIf    = "github.event_name == 'push' && github.ref == 'refs/heads/main' && needs.nix.outputs.vendor_hash_bumped == 'true'"
	refuseIf  = "steps.vendorhash.outputs.bumped == 'true' && ((github.event_name == 'push' && github.ref != 'refs/heads/main') || (github.event_name == 'pull_request' && startsWith(github.base_ref, 'release-')))"
	refuseMsg = `echo "::error::nix/package.nix has a stale vendorHash at $GITHUB_SHA on $GITHUB_REF_NAME, and only main is healed. Bring main's healed vendorHash into this branch and push again."`
	// The same step's message for a pull request into release-*, which names its
	// base; the push message above is kept as it was.
	refusePRMsg = `echo "::error::nix/package.nix has a stale vendorHash in this pull request into $GITHUB_BASE_REF, and only main is healed. Commit the vendorHash printed by the Recompute step above into nix/package.nix and push again."`
	stopLine    = `echo "# STOP THE LINE: a race run on $GITHUB_REF_NAME failed or did not finish"`
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
						lineAllow = append(lineAllow, refuseMsg, refusePRMsg)
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
