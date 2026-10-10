package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TASK-1391: depages dates what a change adds and fails on anything younger
// than --days. These pin each source, the base diff, the allowlist and the
// exit codes against fake registries; no test touches the network.

func TestParseGoSumSkipsGoModOnlyLines(t *testing.T) {
	got := parseGoSum([]byte("example.com/a v1.2.0 h1:x=\nexample.com/a v1.2.0/go.mod h1:y=\nexample.com/b v0.0.0-20260101000000-abcdef123456 h1:z=\n"))
	want := []Dep{{"go", "example.com/a", "v1.2.0"}, {"go", "example.com/b", "v0.0.0-20260101000000-abcdef123456"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}
}

func TestParsePackageLockTakesRegistryPackagesOnly(t *testing.T) {
	lock := `{"lockfileVersion":3,"packages":{
		"":{"name":"web"},
		"node_modules/svelte":{"version":"5.1.0","resolved":"https://registry.npmjs.org/svelte/-/svelte-5.1.0.tgz"},
		"node_modules/@tiptap/core":{"version":"3.31.4","resolved":"https://registry.npmjs.org/@tiptap/core/-/core-3.31.4.tgz"},
		"node_modules/a/node_modules/b":{"version":"1.0.0","resolved":"https://registry.npmjs.org/b/-/b-1.0.0.tgz"},
		"node_modules/gitdep":{"version":"1.0.0","resolved":"git+https://github.com/x/y.git#abc"},
		"node_modules/linked":{"link":true,"resolved":"../linked"},
		"node_modules/aliased":{"name":"real-pkg","version":"2.0.0","resolved":"https://registry.npmjs.org/real-pkg/-/real-pkg-2.0.0.tgz"}
	}}`
	got, err := parsePackageLock([]byte(lock))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, d := range uniq(got) {
		keys[d.key()] = true
	}
	for _, want := range []string{"npm svelte@5.1.0", "npm @tiptap/core@3.31.4", "npm b@1.0.0", "npm real-pkg@2.0.0"} {
		if !keys[want] {
			t.Errorf("missing %s in %v", want, keys)
		}
	}
	if len(keys) != 4 {
		t.Errorf("got %v, want exactly the four registry packages", keys)
	}
	if _, err := parsePackageLock([]byte(`{"lockfileVersion":1,"dependencies":{}}`)); err == nil {
		t.Error("a v1 lockfile was read as empty instead of refused")
	}
}

func TestTokenGoesOnlyToTheTokenHost(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	src := Sources{GitHubAPI: srv.URL, Token: "tok", TokenHost: "api.github.com", Client: srv.Client()}
	var v map[string]any
	_ = getJSON(src, srv.URL+"/x", true, &v)
	if got != "" {
		t.Fatalf("the token was sent to %s: %q", srv.URL, got)
	}
}

func TestParseWorkflowSharesTheRepoSHA(t *testing.T) {
	sha := strings.Repeat("a", 40)
	got := uniq(parseWorkflow([]byte("- uses: actions/cache/save@" + sha + " # v4\n- uses: actions/cache/restore@" + sha + "\n- uses: ./local\n- uses: actions/checkout@v4\n")))
	if len(got) != 1 || got[0] != (Dep{"action", "actions/cache", sha}) {
		t.Fatalf("got %+v", got)
	}
}

func TestEscapeModule(t *testing.T) {
	if got := escapeModule("github.com/BurntSushi/toml"); got != "github.com/!burnt!sushi/toml" {
		t.Fatalf("got %q", got)
	}
}

func TestSubtractKeepsOnlyNewPairs(t *testing.T) {
	head := []Dep{{"npm", "a", "2.0.0"}, {"npm", "b", "1.0.0"}}
	base := []Dep{{"npm", "a", "1.0.0"}, {"npm", "b", "1.0.0"}}
	got := subtract(head, base)
	if len(got) != 1 || got[0].key() != "npm a@2.0.0" {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadAllowRequiresAReason(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "allow.txt")
	os.WriteFile(p, []byte("# comment\nnpm svelte@5.1.0  security fix for CVE-X\n"), 0o644)
	got, err := loadAllow(p)
	if err != nil || got["npm svelte@5.1.0"] != "security fix for CVE-X" {
		t.Fatalf("got %v %v", got, err)
	}
	os.WriteFile(p, []byte("npm svelte@5.1.0\n"), 0o644)
	if _, err := loadAllow(p); err == nil {
		t.Fatal("a line without a reason was accepted")
	}
	if got, err := loadAllow(filepath.Join(dir, "absent")); err != nil || len(got) != 0 {
		t.Fatalf("absent file: %v %v", got, err)
	}
}

// fakeSources serves one publish time per dependency from fake endpoints.
func fakeSources(t *testing.T, times map[string]time.Time, sawToken *string) Sources {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/proxy/", func(w http.ResponseWriter, r *http.Request) {
		// /proxy/<escaped module>/@v/<version>.info
		path := strings.TrimPrefix(r.URL.Path, "/proxy/")
		i := strings.Index(path, "/@v/")
		key := "go " + path[:i] + "@" + strings.TrimSuffix(path[i+4:], ".info")
		ts, ok := times[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Version": "x", "Time": ts})
	})
	mux.HandleFunc("/npm/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/npm/")
		tm := map[string]time.Time{}
		for k, v := range times {
			if strings.HasPrefix(k, "npm "+name+"@") {
				tm[strings.TrimPrefix(k, "npm "+name+"@")] = v
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"time": tm})
	})
	mux.HandleFunc("/gh/repos/", func(w http.ResponseWriter, r *http.Request) {
		if sawToken != nil {
			*sawToken = r.Header.Get("Authorization")
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/gh/repos/"), "/commits/")
		ts, ok := times["action "+parts[0]+"@"+parts[1]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"committer": map[string]any{"date": ts}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return Sources{GoProxy: srv.URL + "/proxy", NPM: srv.URL + "/npm", GitHubAPI: srv.URL + "/gh", Token: "tok", TokenHost: host, Client: srv.Client()}
}

func TestLookupsAndReport(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	sha := strings.Repeat("b", 40)
	times := map[string]time.Time{
		"go github.com/!burnt!sushi/toml@v1.5.0": now.AddDate(0, 0, -30),
		"npm @tiptap/core@3.31.4":                now.AddDate(0, 0, -2),
		"npm svelte@5.1.0":                       now.AddDate(0, 0, -40),
		"action actions/cache@" + sha:            now.AddDate(0, 0, -1),
	}
	var tok string
	src := fakeSources(t, times, &tok)
	deps := []Dep{
		{"go", "github.com/BurntSushi/toml", "v1.5.0"},
		{"npm", "@tiptap/core", "3.31.4"},
		{"npm", "svelte", "5.1.0"},
		{"action", "actions/cache", sha},
		{"npm", "ghost", "1.0.0"}, // the registry has no time for it
	}
	results := lookupAll(src, deps, 2)
	if tok != "Bearer tok" {
		t.Errorf("GitHub token not sent: %q", tok)
	}

	var out bytes.Buffer
	if code := report(results, map[string]string{}, 7, now, &out); code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out.String())
	}
	s := out.String()
	for _, want := range []string{"npm @tiptap/core@3.31.4", "action actions/cache@" + sha, "could not be dated", "npm ghost@1.0.0"} {
		if !strings.Contains(s, want) {
			t.Errorf("report lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(strings.SplitN(s, "younger than 7 days:", 2)[1], "svelte") {
		t.Errorf("an old package was reported young:\n%s", s)
	}

	// Allowing both young ones turns the run green; the unknown stays a warning.
	allowed := map[string]string{"npm @tiptap/core@3.31.4": "coordinated bump", "action actions/cache@" + sha: "security fix"}
	out.Reset()
	if code := report(results, allowed, 7, now, &out); code != 0 {
		t.Fatalf("exit %d with both allowed:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "allowed: coordinated bump") {
		t.Errorf("allowed entries not listed:\n%s", out.String())
	}
}

// --base checks only what is new since the ref, read from a real git repo.
func TestRunAgainstABase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(p, s string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644)
	}
	shaOld, shaNew := strings.Repeat("a", 40), strings.Repeat("c", 40)
	lock := func(v string) string {
		return `{"lockfileVersion":3,"packages":{"node_modules/pkg":{"version":"` + v + `","resolved":"https://registry.npmjs.org/pkg/-/pkg.tgz"}}}`
	}
	git("init", "-q")
	write("go.sum", "example.com/old v1.0.0 h1:x=\n")
	write("web/package-lock.json", lock("1.0.0"))
	write(".github/workflows/ci.yml", "- uses: acme/act@"+shaOld+"\n")
	git("add", ".")
	git("commit", "-qm", "base")
	write("go.sum", "example.com/old v1.0.0 h1:x=\nexample.com/new v2.0.0 h1:y=\n")
	write("web/package-lock.json", lock("1.1.0"))
	write(".github/actions/local/action.yml", "- uses: acme/act@"+shaNew+"\n")

	now := time.Now()
	src := fakeSources(t, map[string]time.Time{
		"go example.com/old@v1.0.0": now.AddDate(0, 0, -1), // young, but not new: not checked
		"go example.com/new@v2.0.0": now.AddDate(0, 0, -100),
		"npm pkg@1.1.0":             now.AddDate(0, 0, -100),
		"action acme/act@" + shaOld: now.AddDate(0, 0, -1),
		"action acme/act@" + shaNew: now.AddDate(0, 0, -100),
	}, nil)
	t.Setenv("DEPAGES_GOPROXY", src.GoProxy)
	t.Setenv("DEPAGES_NPM", src.NPM)
	t.Setenv("DEPAGES_GITHUB_API", src.GitHubAPI)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--root", dir, "--base", "HEAD"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s %s", code, stdout.String(), stderr.String())
	}
	// The new module, the bumped npm package and the composite action's new
	// pin: three, none of them young.
	if !strings.Contains(stdout.String(), "checked 3 dependencies") {
		t.Errorf("want the three new dependencies checked:\n%s", stdout.String())
	}
	// Without --base the young old module is checked and fails the run.
	stdout.Reset()
	if code := run([]string{"--root", dir}, &stdout, &stderr); code != 1 {
		t.Fatalf("full scan exit %d, want 1:\n%s", code, stdout.String())
	}
}
