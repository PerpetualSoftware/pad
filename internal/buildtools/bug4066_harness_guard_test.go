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

// testTempBase is the directory t.TempDir() creates its per-test dirs in
// (<base>/<TestName><random>/<NNN>), read from a real t.TempDir() rather than
// os.TempDir(), which need not be the same.
func testTempBase(t *testing.T) string {
	t.Helper()
	return filepath.Dir(filepath.Dir(t.TempDir()))
}

// insideTestTempDir reports whether p is inside a Go test's temp dir: under
// base, in a top-level directory named for a test (t.TempDir()'s
// "<TestName><random>"). Any test's counts, so a subtest may use its
// parent's.
func insideTestTempDir(base, p string) bool {
	rel, err := filepath.Rel(base, filepath.Clean(p))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	return strings.HasPrefix(strings.SplitN(rel, string(filepath.Separator), 2)[0], "Test")
}

// requireSafeScriptEnv is the harness guard every install-refresh exec in
// this package passes through BEFORE the script runs (BUG-4066 incident; lead
// ruling on the BUG-4066 trail). A test whose environment could let the
// script target the box's REAL shared server fails here, without running.
func requireSafeScriptEnv(t *testing.T, env []string, dir string) {
	t.Helper()
	if err := scriptEnvHazard(env, dir, testTempBase(t)); err != nil {
		t.Fatalf("HARNESS GUARD (BUG-4066): refusing to run install-refresh: %v", err)
	}
}

var configPortLine = regexp.MustCompile(`(?m)^[[:space:]]*port[[:space:]]*=[[:space:]]*"?([0-9]+)`)

// scriptEnvHazard reports why env is unsafe for an install-refresh run:
//   - no port the script would resolve without falling back to its default
//     (PAD_PORT unset or non-numeric, and no port in the test's own data dir's
//     config.toml): the default is 7777, the real server;
//   - a resolved port of 7777;
//   - a resolved port that answers on a LAN address and is not this test's
//     own stub (the stub serves /stub/pid; a real pad does not). A stub bound
//     to 0.0.0.0 by a test answers on the LAN address legitimately.
func scriptEnvHazard(env []string, dir, tempBase string) error {
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
	if !filepath.IsAbs(dataDir) {
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
	if !insideTestTempDir(tempBase, home) {
		return fmt.Errorf("HOME %q is not inside a test temp dir", home)
	}
	if !insideTestTempDir(tempBase, dataDir) {
		return fmt.Errorf("the data dir %q is not inside a test temp dir", dataDir)
	}
	port := get("PAD_PORT")
	if !numeric(port) {
		b, _ := os.ReadFile(filepath.Join(dataDir, "config.toml"))
		m := configPortLine.FindSubmatch(b)
		if m == nil {
			return fmt.Errorf("PAD_PORT is %q and %s names no port, so the script would fall back to its default 7777: the box's real server", port, filepath.Join(dataDir, "config.toml"))
		}
		port = string(m[1])
	}
	if port == "7777" {
		return fmt.Errorf("the resolved port is 7777: the box's real server")
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
		if !answersAsStub(port, get("STUB_TOKEN")) {
			return fmt.Errorf("port %s answers on the LAN address %s and is not this test's stub", port, ipn.IP)
		}
	}
	return nil
}

// answersAsStub: condition 2 (lead ruling), the port echoes THIS test's
// random token, not merely any /stub/pid.
func answersAsStub(port, token string) bool {
	if token == "" {
		return false
	}
	client := &http.Client{Timeout: time.Second}
	res, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/stub/token")
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
	if err := scriptEnvHazard(append(base, "PAD_PORT="), "", testTempBase(t)); err == nil {
		t.Errorf("PAD_PORT unset with no config port was allowed")
	}
	if err := scriptEnvHazard(append(base, "PAD_PORT=not-a-number"), "", testTempBase(t)); err == nil {
		t.Errorf("a non-numeric PAD_PORT with no config port was allowed")
	}
	if err := scriptEnvHazard(append(base, "PAD_PORT=7777"), "", testTempBase(t)); err == nil {
		t.Errorf("PAD_PORT=7777 was allowed")
	}
	if err := scriptEnvHazard(append(base, "PAD_PORT="+free), "", testTempBase(t)); err != nil {
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
	if err := scriptEnvHazard(append(base, "PAD_PORT="), "", testTempBase(t)); err != nil {
		t.Errorf("a config-file port was refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("port = 7777\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scriptEnvHazard(append(base, "PAD_PORT="), "", testTempBase(t)); err == nil {
		t.Errorf("a config-file port of 7777 was allowed")
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
	if err := scriptEnvHazard(append(base, "PAD_PORT="+lport), "", testTempBase(t)); err == nil {
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
			if err := scriptEnvHazard(tc.env, "", testTempBase(t)); err == nil {
				t.Fatalf("allowed: %v", tc.env)
			}
		})
	}
	if err := scriptEnvHazard([]string{"HOME=" + tmp, "PAD_DATA_DIR=data", "PAD_DB_PATH=", "PAD_PORT=" + free}, tmp, testTempBase(t)); err != nil {
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
	if err := scriptEnvHazard(append(base, "STUB_TOKEN=this-tests-token"), "", testTempBase(t)); err == nil {
		t.Errorf("a LAN answer with another test's token was allowed")
	}
	if err := scriptEnvHazard(base, "", testTempBase(t)); err == nil {
		t.Errorf("a LAN answer with no token at all was allowed")
	}
	if err := scriptEnvHazard(append(base, "STUB_TOKEN="+served), "", testTempBase(t)); err != nil {
		t.Errorf("this test's own token was refused: %v", err)
	}
}
