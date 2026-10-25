package buildtools

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubTokens holds one random token per test HOME: stubEnv.env() hands it to
// every stub the test starts (STUB_TOKEN), and the guard requires it back.
var stubTokens sync.Map

func stubTokenFor(home string) string {
	if v, ok := stubTokens.Load(home); ok {
		return v.(string)
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	v, _ := stubTokens.LoadOrStore(home, hex.EncodeToString(b))
	return v.(string)
}

// resolveExisting symlink-resolves the longest existing prefix of p and
// re-joins the rest, so a path not yet created still compares resolved.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// testTempBase is the directory t.TempDir() creates its per-test dirs in
// (<base>/<TestName><random>/<NNN>), read from a real t.TempDir() rather than
// os.TempDir(), which need not be the same.
func testTempBase(t *testing.T) string {
	t.Helper()
	return filepath.Dir(filepath.Dir(t.TempDir()))
}

// tempScope is where THIS test's temp dirs live: the shared base and the
// test's top-level name. t.TempDir() names its dir for the FULL test name cut
// to 64 bytes, symbols such as "/" dropped, then random digits (Go's
// testing.go makeTempDir). So a subtest's is "<Parent><sub…>" and digits, and
// a top-level name of 64 bytes or more appears only as its first 64. top is
// cut the same way, or a long test name is refused by its own temp dir
// (BUG-4068, found when its tests first ran under this guard).
type tempScope struct{ base, top string }

func testTempScope(t *testing.T) tempScope {
	t.Helper()
	top := strings.SplitN(t.Name(), "/", 2)[0]
	return tempScope{testTempBase(t), top[:min(len(top), 64)]}
}

// insideTestTempDir reports whether p is inside this test's temp dir: under
// sc.base, in a top-level directory named for sc.top.
func insideTestTempDir(sc tempScope, p string) bool {
	// Both sides symlink-resolved (BUG-4066 r1): on macOS t.TempDir() is
	// under /var/folders, a symlink to /private/var/folders, and the tests
	// anchor under realDir()'s resolved path.
	rel, err := filepath.Rel(resolveExisting(sc.base), resolveExisting(p))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	// A prefix, not an exact name: t.TempDir drops a subtest's "/" and
	// truncates the name, so a subtest's directory is "<Parent><sub…>" and
	// digits. That also admits a longer test name sharing the prefix
	// (TestFoo's scope accepts TestFooBar's directory), which no separator
	// in the directory name can rule out.
	return strings.HasPrefix(strings.SplitN(rel, string(filepath.Separator), 2)[0], sc.top)
}

// requireSafeScriptEnv is the harness guard every install-refresh exec in
// this package passes through BEFORE the script runs (BUG-4066 incident; lead
// ruling on the BUG-4066 trail). A test whose environment could let the
// script target the box's REAL shared server fails here, without running.
func requireSafeScriptEnv(t *testing.T, env []string, dir string) {
	t.Helper()
	if err := scriptEnvHazard(env, dir, testTempScope(t)); err != nil {
		t.Fatalf("HARNESS GUARD (BUG-4066): refusing to run install-refresh: %v", err)
	}
}

var configPortLine = regexp.MustCompile(`(?m)^[[:space:]]*port[[:space:]]*=[[:space:]]*(.*)$`)

// configPortValue reads config.toml's port the way the script's config_value
// does: the FIRST port line, unquoted, an inline comment and trailing space
// stripped. Taking leading digits instead read "8080abc" as 8080 where the
// script rejects it and falls back to 7777 (BUG-4066 r3 item 7).
func configPortValue(b []byte) (string, bool) {
	m := configPortLine.FindSubmatch(b)
	if m == nil {
		return "", false
	}
	v := string(m[1])
	if q := regexp.MustCompile(`^"([^"]*)".*$`).FindStringSubmatch(v); q != nil {
		v = q[1]
	}
	v = regexp.MustCompile(`[[:space:]]*#.*$`).ReplaceAllString(v, "")
	return strings.TrimRight(v, " \t\r"), true
}

// scriptEnvHazard reports why env is unsafe for an install-refresh run:
//   - no port the script would resolve without falling back to its default
//     (PAD_PORT unset or non-numeric, and no port in the test's own data dir's
//     config.toml): the default is 7777, the real server;
//   - a resolved port of 7777;
//   - a resolved port that answers on a LAN address and is not this test's
//     own stub (the stub serves /stub/pid; a real pad does not). A stub bound
//     to 0.0.0.0 by a test answers on the LAN address legitimately.
func scriptEnvHazard(env []string, dir string, scope tempScope) error {
	get := func(key string) string {
		v := ""
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				v = strings.TrimPrefix(kv, key+"=")
			}
		}
		return v
	}
	numeric := func(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }
	home := get("HOME")
	dataDir := filepath.Join(home, ".pad")
	if v := get("PAD_DATA_DIR"); v != "" {
		dataDir = v
	}
	if v := get("PAD_DB_PATH"); v != "" {
		dataDir = filepath.Dir(v)
	}
	relativeDataDir := !filepath.IsAbs(dataDir)
	if relativeDataDir {
		base := dir
		if base == "" {
			base, _ = os.Getwd()
		}
		dataDir = filepath.Join(base, dataDir)
	}
	// Condition 1 (lead ruling): HOME and the data dir are the test's own,
	// never the real user's home and never outside a test temp dir.
	if real, err := os.UserHomeDir(); err == nil && real != "" {
		if filepath.Clean(home) == filepath.Clean(real) || filepath.Clean(dataDir) == filepath.Join(real, ".pad") {
			return fmt.Errorf("HOME (%s) or the data dir (%s) is the real user's", home, dataDir)
		}
	}
	if !insideTestTempDir(scope, home) {
		return fmt.Errorf("HOME %q is not inside a test temp dir", home)
	}
	if !insideTestTempDir(scope, dataDir) {
		return fmt.Errorf("the data dir %q is not inside a test temp dir", dataDir)
	}
	// PAD_HOST is inherited from the developer's shell; only empty or
	// loopback keeps the script on this machine's own stub (BUG-4066 r2).
	if h := strings.Trim(get("PAD_HOST"), "[]"); h != "" && h != "localhost" {
		if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("PAD_HOST is %q: not empty or loopback, so the script could target another host", get("PAD_HOST"))
		}
	}
	port := get("PAD_PORT")
	if !numeric(port) && relativeDataDir {
		// BUG-4066 r1: after the capture the script reads config.toml under
		// <SERVER_CWD>/<dir>, which the guard cannot know before exec.
		return fmt.Errorf("PAD_PORT is %q and the data dir %q is relative: the script would read the port from the server's cwd, which this guard cannot see", port, get("PAD_DATA_DIR")+get("PAD_DB_PATH"))
	}
	if !numeric(port) {
		b, _ := os.ReadFile(filepath.Join(dataDir, "config.toml"))
		v, ok := configPortValue(b)
		if !ok {
			return fmt.Errorf("PAD_PORT is %q and %s names no port, so the script would fall back to its default 7777: the box's real server", port, filepath.Join(dataDir, "config.toml"))
		}
		port = v
	}
	// Numeric, so "07777" is refused too, and before any dial: this guard
	// never dials port 7777 under any input (BUG-4066 r2).
	if n, err := strconv.Atoi(port); err != nil || n == 7777 {
		return fmt.Errorf("the resolved port %q is 7777 (or unreadable): the box's real server", port)
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ipn.IP.String(), port), 300*time.Millisecond)
		if err != nil {
			continue
		}
		_ = c.Close()
		if !answersAsStub(ipn.IP.String(), port, get("STUB_TOKEN")) {
			return fmt.Errorf("port %s answers on the LAN address %s and is not this test's stub", port, ipn.IP)
		}
	}
	return nil
}

// answersAsStub: condition 2 (lead ruling), the port echoes THIS test's
// random token, not merely any /stub/pid.
func answersAsStub(ip, port, token string) bool {
	if token == "" {
		return false
	}
	// Asked on the LAN address that answered (BUG-4066 r1), not on loopback,
	// which could be a different process holding the same port number.
	client := &http.Client{Timeout: time.Second}
	res, err := client.Get("http://" + net.JoinHostPort(ip, port) + "/stub/token")
	if err != nil {
		return false
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode == http.StatusOK && string(b) == token
}

// The guard itself, never demonstrated against the live server: no row execs
// the script, and the 7777 row is refused on the number alone.
func TestHarnessGuard_RefusesAHazardousScriptEnv(t *testing.T) {
	home := t.TempDir()
	free := fmt.Sprint(freePort(t))
	base := []string{"HOME=" + home, "PAD_DATA_DIR=", "PAD_DB_PATH="}
	if err := scriptEnvHazard(append(base, "PAD_PORT="), "", testTempScope(t)); err == nil {
		t.Errorf("PAD_PORT unset with no config port was allowed")
	}
	if err := scriptEnvHazard(append(base, "PAD_PORT=not-a-number"), "", testTempScope(t)); err == nil {
		t.Errorf("a non-numeric PAD_PORT with no config port was allowed")
	}
	// Asserted by message, so the result does not depend on what listens on
	// 7777 (a string compare would reach the LAN dial and be refused there).
	for _, p := range []string{"7777", "07777", "0007777"} {
		err := scriptEnvHazard(append(base, "PAD_PORT="+p), "", testTempScope(t))
		if err == nil || !strings.Contains(err.Error(), "is 7777 (or unreadable)") {
			t.Errorf("PAD_PORT=%s: want the numeric 7777 refusal, got %v", p, err)
		}
	}
	for _, h := range []string{"192.0.2.1", "0.0.0.0", "example.com", "::"} {
		err := scriptEnvHazard(append(base, "PAD_PORT="+free, "PAD_HOST="+h), "", testTempScope(t))
		if err == nil || !strings.Contains(err.Error(), "PAD_HOST") {
			t.Errorf("PAD_HOST=%s: want the PAD_HOST refusal, got %v", h, err)
		}
	}
	for _, h := range []string{"", "127.0.0.1", "localhost", "::1", "[::1]"} {
		if err := scriptEnvHazard(append(base, "PAD_PORT="+free, "PAD_HOST="+h), "", testTempScope(t)); err != nil {
			t.Errorf("PAD_HOST=%q was refused: %v", h, err)
		}
	}
	// A relative data dir with the port left to config.toml: the script reads
	// it from the server's cwd, which the guard cannot see (BUG-4066 r1).
	relHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(relHome, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relHome, "data", "config.toml"), []byte("port = "+free+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scriptEnvHazard([]string{"HOME=" + relHome, "PAD_DATA_DIR=data", "PAD_DB_PATH=", "PAD_PORT="}, relHome, testTempScope(t)); err == nil {
		t.Errorf("a relative data dir with a config-sourced port was allowed")
	}
	if err := scriptEnvHazard(append(base, "PAD_PORT="+free), "", testTempScope(t)); err != nil {
		t.Errorf("a free private port was refused: %v", err)
	}
	// A port from the test's own config file is a resolved port.
	cfg := filepath.Join(home, ".pad")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("port = "+free+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scriptEnvHazard(append(base, "PAD_PORT="), "", testTempScope(t)); err != nil {
		t.Errorf("a config-file port was refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("port = 7777\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scriptEnvHazard(append(base, "PAD_PORT="), "", testTempScope(t)); err == nil || !strings.Contains(err.Error(), "is 7777 (or unreadable)") {
		t.Errorf("a config-file port of 7777: want the numeric 7777 refusal, got %v", err)
	}
	// Something that is not a stub, answering on a LAN address.
	var lan string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			lan = ipn.IP.String()
			break
		}
	}
	if lan == "" {
		t.Log("no LAN IPv4 address: the LAN row is skipped")
		return
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(lan, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	lport := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	if err := scriptEnvHazard(append(base, "PAD_PORT="+lport), "", testTempScope(t)); err == nil {
		t.Errorf("a non-stub answering on the LAN address %s:%s was allowed", lan, lport)
	}
}

// Condition 1 (lead ruling): HOME and the data dir must be the test's own.
func TestHarnessGuard_RefusesARealOrForeignHome(t *testing.T) {
	free := fmt.Sprint(freePort(t))
	real, err := os.UserHomeDir()
	if err != nil || real == "" {
		t.Skip("no user home")
	}
	tmp := t.TempDir()
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{"HOME is the real user's", []string{"HOME=" + real, "PAD_DATA_DIR=" + tmp, "PAD_DB_PATH=", "PAD_PORT=" + free}},
		{"data dir is the real ~/.pad", []string{"HOME=" + tmp, "PAD_DATA_DIR=" + filepath.Join(real, ".pad"), "PAD_DB_PATH=", "PAD_PORT=" + free}},
		{"HOME outside any test temp dir", []string{"HOME=" + testTempBase(t), "PAD_DATA_DIR=", "PAD_DB_PATH=", "PAD_PORT=" + free}},
		{"data dir outside any test temp dir", []string{"HOME=" + tmp, "PAD_DATA_DIR=/var/tmp/pad-guard-row", "PAD_DB_PATH=", "PAD_PORT=" + free}},
		{"relative data dir against the package cwd", []string{"HOME=" + tmp, "PAD_DATA_DIR=data", "PAD_DB_PATH=", "PAD_PORT=" + free}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := scriptEnvHazard(tc.env, "", testTempScope(t)); err == nil {
				t.Fatalf("allowed: %v", tc.env)
			}
		})
	}
	if err := scriptEnvHazard([]string{"HOME=" + tmp, "PAD_DATA_DIR=data", "PAD_DB_PATH=", "PAD_PORT=" + free}, tmp, testTempScope(t)); err != nil {
		t.Fatalf("a relative data dir anchored in the test's own dir was refused: %v", err)
	}
}

// Condition 2 (lead ruling): only THIS test's token exempts a LAN answer.
func TestHarnessGuard_StubTokenMustMatch(t *testing.T) {
	ln, err := net.Listen("tcp", ":0") // every interface, as a 0.0.0.0 stub binds
	if err != nil {
		t.Fatal(err)
	}
	const served = "a-token-from-some-other-test"
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stub/token" {
			fmt.Fprint(w, served)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	lan := false
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			lan = true
		}
	}
	if !lan {
		t.Skip("no LAN IPv4 address")
	}
	home := t.TempDir()
	base := []string{"HOME=" + home, "PAD_DATA_DIR=", "PAD_DB_PATH=", "PAD_PORT=" + port}
	if err := scriptEnvHazard(append(base, "STUB_TOKEN=this-tests-token"), "", testTempScope(t)); err == nil {
		t.Errorf("a LAN answer with another test's token was allowed")
	}
	if err := scriptEnvHazard(base, "", testTempScope(t)); err == nil {
		t.Errorf("a LAN answer with no token at all was allowed")
	}
	if err := scriptEnvHazard(append(base, "STUB_TOKEN="+served), "", testTempScope(t)); err != nil {
		t.Errorf("this test's own token was refused: %v", err)
	}
}

// The temp-dir check compares symlink-resolved paths (BUG-4066 r1, macOS).
func TestHarnessGuard_ResolvesSymlinkedTempPaths(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here")
	}
	free := fmt.Sprint(freePort(t))
	// HOME through the symlink, the data dir anchored under the resolved path.
	env := []string{"HOME=" + link, "PAD_DATA_DIR=data", "PAD_DB_PATH=", "PAD_PORT=" + free}
	if err := scriptEnvHazard(env, real, testTempScope(t)); err != nil {
		t.Fatalf("a symlinked test temp path was refused: %v", err)
	}
	// The layout t.TempDir() makes: <base>/<TestName><random>/... . With the
	// base reached through a symlink and the path given resolved, they match.
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "TestSomething123", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	baseLink := filepath.Join(t.TempDir(), "baselink")
	if err := os.Symlink(base, baseLink); err != nil {
		t.Fatal(err)
	}
	if !insideTestTempDir(tempScope{baseLink, "TestSomething"}, filepath.Join(base, "TestSomething123", "x")) {
		t.Fatalf("a resolved path is not inside the same base reached through a symlink")
	}
	if !insideTestTempDir(tempScope{base, "TestSomething"}, filepath.Join(baseLink, "TestSomething123", "x")) {
		t.Fatalf("a symlinked path is not inside its resolved base")
	}
	// Only THIS test's directory (BUG-4066 r2 item 9): another test's is
	// refused; a subtest's, which t.TempDir names "<Parent><sub…>", passes.
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{"TestOther123", false},
		{"TestSomethingsub456", true},
	} {
		if got := insideTestTempDir(tempScope{base, "TestSomething"}, filepath.Join(base, tc.dir, "x")); got != tc.want {
			t.Errorf("%s: inside = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

// unguardedScriptExecs scans Go source for every exec.Command or
// exec.CommandContext whose arguments include scriptPath( and returns the
// 1-based line of each one that is not guarded: requireSafeScriptEnv( must
// appear in the SAME function, after the exec and before that exec's
// variable runs (Run, Output, CombinedOutput or Start). An exec that is never
// assigned to a variable has no later run to wait for, so it is always
// flagged. found counts every install-refresh exec seen.
func unguardedScriptExecs(src string) (bad []int, found int) {
	execRe := regexp.MustCompile(`(?:(\w+)\s*:?=\s*)?exec\.Command(?:Context)?\(.*\bscriptPath\(`)
	lines := strings.Split(src, "\n")
	fnEnd := func(from int) int {
		for j := from + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "func ") {
				return j
			}
		}
		return len(lines)
	}
	for i, line := range lines {
		m := execRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		found++
		v := m[1]
		if v == "" {
			bad = append(bad, i+1)
			continue
		}
		runRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(v) + `\.(Run|Output|CombinedOutput|Start)\(`)
		guarded := false
		for _, next := range lines[i+1 : fnEnd(i)] {
			if strings.Contains(next, "requireSafeScriptEnv(") {
				guarded = true
				break
			}
			if runRe.MatchString(next) {
				break
			}
		}
		if !guarded {
			bad = append(bad, i+1)
		}
	}
	return bad, found
}

// Every exec of install-refresh in this package passes the guard before it
// runs (BUG-4066 r1): a new unguarded exec fails here, by source scan.
func TestHarnessGuard_EveryScriptExecIsGuarded(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		bad, found := unguardedScriptExecs(string(b))
		total += found
		for _, ln := range bad {
			t.Errorf("%s:%d execs install-refresh without requireSafeScriptEnv before it runs", f, ln)
		}
	}
	if total < 14 {
		t.Fatalf("found only %d install-refresh execs; the scan is not seeing them", total)
	}
}

// The scanner itself (BUG-4066 r2): a differently named variable, the
// CommandContext form, and a guard that only appears in a LATER function must
// all be flagged; a guarded one must not. The snippets spell the exec call as
// EXEC so this file's own scan does not see them.
func TestHarnessGuard_ScannerFlagsAnUnguardedExec(t *testing.T) {
	snip := func(s string) string {
		return strings.ReplaceAll(s, "EXEC", "exec.Command")
	}
	for _, tc := range []struct {
		name, src string
		want      int
	}{
		{"other variable name, guard in a later function", snip(`func a(t *testing.T) {
	c := EXEC("bash", scriptPath(t), x)
	c.Env = env
	_ = c.Run()
}

func b(t *testing.T) {
	requireSafeScriptEnv(t, nil, "")
}
`), 1},
		{"never run in its function, guard in a later function", snip(`func a(t *testing.T) *exec.Cmd {
	c := EXEC("bash", scriptPath(t), x)
	return c
}

func b(t *testing.T) {
	requireSafeScriptEnv(t, nil, "")
}
`), 1},
		{"CommandContext form", snip(`func a(t *testing.T) {
	c := EXECContext(ctx, "bash", scriptPath(t), x)
	out, _ := c.CombinedOutput()
}
`), 1},
		{"guard after the run", snip(`func a(t *testing.T) {
	cmd := EXEC("bash", scriptPath(t))
	cmd.Start()
	requireSafeScriptEnv(t, nil, "")
}
`), 1},
		{"guarded", snip(`func a(t *testing.T) {
	c := EXEC("bash", scriptPath(t))
	requireSafeScriptEnv(t, c.Env, c.Dir)
	_ = c.Run()
}
`), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad, found := unguardedScriptExecs(tc.src)
			if found != 1 || len(bad) != tc.want {
				t.Fatalf("found=%d flagged=%v, want 1 found and %d flagged", found, bad, tc.want)
			}
		})
	}
}

// The token is asked on the LAN address that answered (BUG-4066 r1): a real
// server on the LAN address and a stub on loopback, sharing one port number,
// must not pass on the stub's token.
func TestHarnessGuard_TokenIsAskedWhereTheLANAnswered(t *testing.T) {
	var lan string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			lan = ipn.IP.String()
			break
		}
	}
	if lan == "" {
		t.Skip("no LAN IPv4 address")
	}
	// The "real" server: on the LAN address only, no token route.
	real, err := net.Listen("tcp", net.JoinHostPort(lan, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	realSrv := &http.Server{Handler: http.NotFoundHandler()}
	go func() { _ = realSrv.Serve(real) }()
	defer realSrv.Close()
	port := fmt.Sprint(real.Addr().(*net.TCPAddr).Port)
	// A stub with this test's token, on loopback, same port number.
	stub, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Skipf("cannot bind loopback on the same port: %v", err)
	}
	defer stub.Close()
	const token = "this-tests-token"
	stubSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, token) })}
	go func() { _ = stubSrv.Serve(stub) }()
	defer stubSrv.Close()
	env := []string{"HOME=" + t.TempDir(), "PAD_DATA_DIR=", "PAD_DB_PATH=", "PAD_PORT=" + port, "STUB_TOKEN=" + token}
	if err := scriptEnvHazard(env, "", testTempScope(t)); err == nil {
		t.Fatalf("a non-stub on %s:%s passed on a loopback stub's token", lan, port)
	}
}

// The guard reads config.toml's port as the script does (BUG-4066 r3 item 7),
// so a value the script rejects, and falls back to 7777 for, is unreadable to
// the guard too, and refused.
func TestHarnessGuard_ReadsTheConfigPortLikeTheScript(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"port = 8080\n", "8080"},
		{"port = 8080 # dev\n", "8080"},
		{"port = \"8080\"\n", "8080"},
		{"  port=8080  \n", "8080"},
		{"port = 8080abc\n", "8080abc"},
		{"port = abc\nport = 8080\n", "abc"},
	} {
		got, ok := configPortValue([]byte(tc.file))
		if !ok || got != tc.want {
			t.Errorf("%q: got %q (found %v), want %q", tc.file, got, ok, tc.want)
		}
	}
	if _, ok := configPortValue([]byte("host = x\n")); ok {
		t.Errorf("a file with no port line read as having one")
	}
	home := t.TempDir()
	cfg := filepath.Join(home, ".pad")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("port = 8080abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := scriptEnvHazard([]string{"HOME=" + home, "PAD_PORT="}, "", testTempScope(t))
	if err == nil || !strings.Contains(err.Error(), "is 7777 (or unreadable)") {
		t.Fatalf("a config port of 8080abc: want the unreadable refusal, got %v", err)
	}
}
