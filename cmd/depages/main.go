// Command depages flags dependencies younger than a threshold (TASK-1391).
//
// It is the companion to Dependabot's cooldown (TASK-1390), which holds a
// release for 7 days before PROPOSING it but cannot see a version added by
// hand (`go get`, `npm install`, an edited workflow pin). depages checks what
// a change adds:
//
//   - Go modules: (module, version) pairs in go.sum,
//   - npm packages: (name, version) pairs in web/package-lock.json,
//   - GitHub Actions: (owner/repo, sha) pins in .github/workflows/*.yml,
//
// dating each from its source of truth (the Go module proxy, the npm registry
// packument, the GitHub commits API) and failing when one is younger than
// --days. With --base <git ref> it checks only the pairs that are new since
// that ref, which is how CI runs it on a pull request; without it, every pair.
//
// Exit status: 0 when nothing is too young (lookup failures are reported as
// warnings and do not fail the run: the check is advisory, and an unreachable
// registry is not evidence about a dependency); 1 when at least one
// dependency is younger than the threshold and not allowed; 2 on a usage or
// local error. Allowed entries live in .github/dep-age-allow.txt, one per
// line: `<go|npm|action> <name>@<version>  <reason>` (the reason is
// required).
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Dep is one dependency version a change can introduce.
type Dep struct {
	Kind    string // "go", "npm" or "action"
	Name    string
	Version string
}

func (d Dep) key() string { return d.Kind + " " + d.Name + "@" + d.Version }

// Result is one dependency's published time, or why it is unknown.
type Result struct {
	Dep       Dep
	Published time.Time
	Err       error
}

// Sources are the lookup endpoints; tests point them at fakes.
type Sources struct {
	GoProxy   string // e.g. https://proxy.golang.org
	NPM       string // e.g. https://registry.npmjs.org
	GitHubAPI string // e.g. https://api.github.com
	Token     string // optional GitHub token
	TokenHost string // the only host the token is sent to (api.github.com)
	Client    *http.Client
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("depages", flag.ContinueOnError)
	fs.SetOutput(stderr)
	days := fs.Int("days", 7, "fail on a dependency published fewer than this many days ago")
	base := fs.String("base", "", "git ref to diff against; only dependencies new since it are checked")
	root := fs.String("root", ".", "repository root")
	allowPath := fs.String("allow", ".github/dep-age-allow.txt", "allowlist file, relative to --root")
	concurrency := fs.Int("concurrency", 16, "parallel lookups")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *days < 1 {
		fmt.Fprintln(stderr, "depages: --days must be at least 1")
		return 2
	}

	read := func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(*root, path)) }
	readBase := func(path string) ([]byte, error) {
		out, err := exec.Command("git", "-C", *root, "show", *base+":"+path).Output()
		if err != nil {
			// A file absent at the base (new in this change) contributes nothing.
			return nil, nil
		}
		return out, nil
	}

	head, err := collect(read, workflowFiles(*root))
	if err != nil {
		fmt.Fprintln(stderr, "depages:", err)
		return 2
	}
	deps := head
	if *base != "" {
		before, err := collect(readBase, workflowFiles(*root))
		if err != nil {
			fmt.Fprintln(stderr, "depages:", err)
			return 2
		}
		deps = subtract(head, before)
	}

	allowed, err := loadAllow(filepath.Join(*root, *allowPath))
	if err != nil {
		fmt.Fprintln(stderr, "depages:", err)
		return 2
	}

	src := Sources{
		GoProxy:   envOr("DEPAGES_GOPROXY", "https://proxy.golang.org"),
		NPM:       envOr("DEPAGES_NPM", "https://registry.npmjs.org"),
		GitHubAPI: envOr("DEPAGES_GITHUB_API", "https://api.github.com"),
		Token:     os.Getenv("GH_TOKEN"),
		TokenHost: "api.github.com",
		Client:    &http.Client{Timeout: 30 * time.Second},
	}
	results := lookupAll(src, deps, *concurrency)
	return report(results, allowed, *days, time.Now(), stdout)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// workflowFiles lists the files whose `uses:` pins a change can add: every
// YAML file under .github (workflows, local actions), and any action.yml or
// action.yaml elsewhere in the repository (a composite action can live
// anywhere; codex rounds 1-2). node_modules and .git are skipped. A
// `docker://` image is not covered: it is not pinned by a commit SHA, and has
// no source of publish time here.
func workflowFiles(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == "node_modules" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		yaml := strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml")
		underGithub := strings.HasPrefix(rel, ".github"+string(filepath.Separator))
		if (yaml && underGithub) || d.Name() == "action.yml" || d.Name() == "action.yaml" {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// collect gathers every dependency the given tree declares.
func collect(read func(string) ([]byte, error), workflows []string) ([]Dep, error) {
	var deps []Dep
	if b, err := read("go.sum"); err == nil && b != nil {
		deps = append(deps, parseGoSum(b)...)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if b, err := read("web/package-lock.json"); err == nil && b != nil {
		npm, err := parsePackageLock(b)
		if err != nil {
			return nil, fmt.Errorf("web/package-lock.json: %w", err)
		}
		deps = append(deps, npm...)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, w := range workflows {
		b, err := read(w)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		deps = append(deps, parseWorkflow(b)...)
	}
	return uniq(deps), nil
}

// parseGoSum returns the (module, version) pairs whose module content go.sum
// pins (the `/go.mod`-only lines name versions whose code is never used).
func parseGoSum(b []byte) []Dep {
	var out []Dep
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || strings.HasSuffix(f[1], "/go.mod") {
			continue
		}
		out = append(out, Dep{Kind: "go", Name: f[0], Version: f[1]})
	}
	return out
}

// parsePackageLock returns the registry packages a lockfile v2/v3 installs.
func parsePackageLock(b []byte) ([]Dep, error) {
	var lock struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Name     string `json:"name"` // set for an alias (`"x": "npm:real@1"`)
			Version  string `json:"version"`
			Resolved string `json:"resolved"`
			Link     bool   `json:"link"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, err
	}
	// A v1 lockfile keeps its tree under `dependencies`, which this does not
	// read; refusing beats checking nothing (codex round 1).
	if lock.LockfileVersion < 2 {
		return nil, fmt.Errorf("lockfileVersion %d is not supported (want 2 or 3)", lock.LockfileVersion)
	}
	var out []Dep
	for path, p := range lock.Packages {
		i := strings.LastIndex(path, "node_modules/")
		if i < 0 || p.Link || p.Version == "" {
			continue // the root package, or a workspace link
		}
		if p.Resolved != "" && !strings.Contains(p.Resolved, "registry.npmjs.org") {
			continue // a git or tarball dependency has no registry publish time
		}
		name := path[i+len("node_modules/"):]
		if p.Name != "" {
			name = p.Name // an alias installs another package under its own name
		}
		out = append(out, Dep{Kind: "npm", Name: name, Version: p.Version})
	}
	return out, nil
}

var usesSHA = regexp.MustCompile(`uses:\s*([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)(?:/[^@\s]*)?@([0-9a-f]{40})`)

// parseWorkflow returns the (owner/repo, sha) pins a workflow uses; a
// sub-action shares its repository's commit.
func parseWorkflow(b []byte) []Dep {
	var out []Dep
	for _, m := range usesSHA.FindAllSubmatch(b, -1) {
		out = append(out, Dep{Kind: "action", Name: string(m[1]), Version: string(m[2])})
	}
	return out
}

func uniq(deps []Dep) []Dep {
	seen := map[string]bool{}
	out := deps[:0:0]
	for _, d := range deps {
		if !seen[d.key()] {
			seen[d.key()] = true
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// subtract returns the dependencies in head that are not in base.
func subtract(head, base []Dep) []Dep {
	had := map[string]bool{}
	for _, d := range base {
		had[d.key()] = true
	}
	var out []Dep
	for _, d := range head {
		if !had[d.key()] {
			out = append(out, d)
		}
	}
	return out
}

var allowLine = regexp.MustCompile(`^(go|npm|action)\s+(\S+)@(\S+)\s+(\S.*)$`)

// loadAllow reads the allowlist; an absent file allows nothing, and a line
// without a reason is an error, so an exception always says why.
func loadAllow(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := allowLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s:%d: want `<go|npm|action> <name>@<version> <reason>`", path, i+1)
		}
		out[Dep{Kind: m[1], Name: m[2], Version: m[3]}.key()] = m[4]
	}
	return out, nil
}

func lookupAll(src Sources, deps []Dep, concurrency int) []Result {
	if concurrency < 1 {
		concurrency = 1
	}
	results := make([]Result, len(deps))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, d := range deps {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, d Dep) {
			defer wg.Done()
			defer func() { <-sem }()
			t, err := published(src, d)
			results[i] = Result{Dep: d, Published: t, Err: err}
		}(i, d)
	}
	wg.Wait()
	return results
}

// escapeModule applies the module proxy's case encoding: an upper-case
// letter becomes '!' plus its lower case.
func escapeModule(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func published(src Sources, d Dep) (time.Time, error) {
	switch d.Kind {
	case "go":
		var info struct{ Time time.Time }
		u := src.GoProxy + "/" + escapeModule(d.Name) + "/@v/" + escapeModule(d.Version) + ".info"
		if err := getJSON(src, u, false, &info); err != nil {
			return time.Time{}, err
		}
		return info.Time, nil
	case "npm":
		// The full packument: only it carries per-version publish times.
		var doc struct{ Time map[string]time.Time }
		u := src.NPM + "/" + strings.Replace(url.PathEscape(d.Name), "%40", "@", 1)
		if err := getJSON(src, u, false, &doc); err != nil {
			return time.Time{}, err
		}
		t, ok := doc.Time[d.Version]
		if !ok {
			return time.Time{}, fmt.Errorf("no publish time for %s", d.Version)
		}
		return t, nil
	case "action":
		var c struct {
			Commit struct {
				Committer struct{ Date time.Time }
			}
		}
		u := src.GitHubAPI + "/repos/" + d.Name + "/commits/" + d.Version
		if err := getJSON(src, u, true, &c); err != nil {
			return time.Time{}, err
		}
		return c.Commit.Committer.Date, nil
	}
	return time.Time{}, fmt.Errorf("unknown kind %q", d.Kind)
}

func getJSON(src Sources, u string, github bool, v any) error {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	// The token goes to GitHub's API host and nowhere else, whatever
	// DEPAGES_GITHUB_API says (codex round 1).
	if github && src.Token != "" && req.URL.Host == src.TokenHost {
		req.Header.Set("Authorization", "Bearer "+src.Token)
	}
	resp, err := src.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// report prints the findings and returns the exit status.
func report(results []Result, allowed map[string]string, days int, now time.Time, w io.Writer) int {
	limit := time.Duration(days) * 24 * time.Hour
	var fresh, allowedFresh, unknown, allowLines []string
	for _, r := range results {
		switch {
		case r.Err != nil:
			unknown = append(unknown, fmt.Sprintf("  %s: %v", r.Dep.key(), r.Err))
		case now.Sub(r.Published) < limit:
			age := now.Sub(r.Published).Round(time.Hour)
			line := fmt.Sprintf("  %-60s published %s (%s ago)", r.Dep.key(), r.Published.UTC().Format("2006-01-02"), age)
			if reason, ok := allowed[r.Dep.key()]; ok {
				allowedFresh = append(allowedFresh, line+"  allowed: "+reason)
			} else {
				fresh = append(fresh, line)
				allowLines = append(allowLines, "  "+r.Dep.key()+"  <reason, e.g. a security fix>")
			}
		}
	}
	fmt.Fprintf(w, "depages: checked %d dependencies against a %d-day threshold\n", len(results), days)
	if len(allowedFresh) > 0 {
		fmt.Fprintf(w, "\nyounger than %d days, ALLOWED (.github/dep-age-allow.txt):\n%s\n", days, strings.Join(allowedFresh, "\n"))
	}
	if len(unknown) > 0 {
		fmt.Fprintf(w, "\nWARNING: %d could not be dated (not a failure):\n%s\n", len(unknown), strings.Join(unknown, "\n"))
	}
	if len(fresh) > 0 {
		fmt.Fprintf(w, "\nyounger than %d days:\n%s\n", days, strings.Join(fresh, "\n"))
		fmt.Fprintf(w, "\nThis check is advisory and does not block a merge. Wait for these to age, or,\n"+
			"when a young version is deliberate (a security fix, say), allow it by adding its\n"+
			"line to .github/dep-age-allow.txt with the reason in place of <...>:\n%s\n", strings.Join(allowLines, "\n"))
		return 1
	}
	return 0
}
