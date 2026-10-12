package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
)

// TASK-3612: moving a workspace to Cloud ends with
// `pad workspace link <slug> --url https://app.getpad.dev` in each project
// directory. Those directories are already linked to the self-hosted server
// (their .pad.toml pins its url), and after an import Cloud usually has the
// SAME slug. link used to answer "Already linked" from the old server and
// change nothing; it also rewrote the whole file, dropping agent_name and the
// [push] consent table, whenever it did write.

// linkServer answers the two calls link makes, for one workspace slug, and
// records which paths it was asked for.
type linkServer struct {
	mu    sync.Mutex
	paths []string
	srv   *httptest.Server
}

func newLinkServer(t *testing.T, slug string) *linkServer {
	t.Helper()
	ls := &linkServer{}
	ws := map[string]any{"id": "w-" + slug, "name": "Acme", "slug": slug, "owner_username": "dave"}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		ls.paths = append(ls.paths, r.URL.Path)
		ls.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/workspaces":
			_ = json.NewEncoder(w).Encode([]any{ws})
		case "/api/v1/workspaces/" + slug:
			_ = json.NewEncoder(w).Encode(ws)
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "not_found", "message": "no"}})
		}
	}))
	t.Cleanup(ls.srv.Close)
	return ls
}

func (ls *linkServer) asked() []string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return append([]string(nil), ls.paths...)
}

// A self-hosted project's link, with the settings a relink must keep.
func writeSelfHostLink(t *testing.T, dir, selfHostURL string) {
	t.Helper()
	body := "workspace = \"acme\"\n" +
		"url = \"" + selfHostURL + "\"\n" +
		"agent_name = \"claude-code\"\n" +
		"future_key = \"kept\"\n\n" +
		"[push]\n  auto_arm = true\n"
	if err := os.WriteFile(filepath.Join(dir, ".pad.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readLink(t *testing.T, dir string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if _, err := toml.DecodeFile(filepath.Join(dir, ".pad.toml"), &doc); err != nil {
		t.Fatalf("read .pad.toml: %v", err)
	}
	return doc
}

func runLink(t *testing.T, dir, url string, args ...string) (string, error) {
	t.Helper()
	setTempHomeMain(t)
	t.Setenv("PAD_TOKEN", "pad_testtoken")
	t.Chdir(dir)
	origURL, origWS := urlFlag, workspaceFlag
	t.Cleanup(func() { urlFlag, workspaceFlag = origURL, origWS })
	urlFlag, workspaceFlag = url, ""
	cmd := linkCmd()
	cmd.SetArgs(args)
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	return out, err
}

func TestLinkRepointsASelfHostLinkToCloudKeepingItsSettings(t *testing.T) {
	selfHost := newLinkServer(t, "acme")
	cloud := newLinkServer(t, "acme")
	dir := t.TempDir()
	writeSelfHostLink(t, dir, selfHost.srv.URL)

	out, err := runLink(t, dir, cloud.srv.URL, "acme")
	if err != nil {
		t.Fatalf("link: %v\n%s", err, out)
	}
	if strings.Contains(out, "Already linked") {
		t.Fatalf("link answered from the old link instead of re-pointing:\n%s", out)
	}

	doc := readLink(t, dir)
	if doc["url"] != cloud.srv.URL {
		t.Errorf("url = %v, want the Cloud server %s", doc["url"], cloud.srv.URL)
	}
	if doc["workspace"] != "acme" {
		t.Errorf("workspace = %v, want acme", doc["workspace"])
	}
	if doc["agent_name"] != "claude-code" {
		t.Errorf("agent_name = %v, want it kept", doc["agent_name"])
	}
	if doc["future_key"] != "kept" {
		t.Errorf("future_key = %v, want an unknown key kept", doc["future_key"])
	}
	push, _ := doc["push"].(map[string]any)
	if push == nil || push["auto_arm"] != true {
		t.Errorf("[push] = %v, want auto_arm = true kept", doc["push"])
	}
	if len(cloud.asked()) == 0 {
		t.Error("link never asked the Cloud server")
	}
	if len(selfHost.asked()) != 0 {
		t.Errorf("link asked the old server %v; --url should have replaced it", selfHost.asked())
	}
}

// The control: with no --url the directory's own pin is the target, so an
// existing link that resolves there is left exactly as it is.
func TestLinkWithoutURLLeavesAWorkingLinkAlone(t *testing.T) {
	selfHost := newLinkServer(t, "acme")
	dir := t.TempDir()
	writeSelfHostLink(t, dir, selfHost.srv.URL)
	before, err := os.ReadFile(filepath.Join(dir, ".pad.toml"))
	if err != nil {
		t.Fatal(err)
	}

	out, err := runLink(t, dir, "", "acme")
	if err != nil {
		t.Fatalf("link: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Already linked") {
		t.Errorf("want Already linked, got:\n%s", out)
	}
	after, err := os.ReadFile(filepath.Join(dir, ".pad.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf(".pad.toml changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if len(selfHost.asked()) == 0 {
		t.Error("link never asked the pinned server")
	}
}

func TestSameServerURL(t *testing.T) {
	t.Parallel()
	same := [][2]string{
		{"", ""},
		{"https://app.getpad.dev", "https://app.getpad.dev/"},
		{"https://app.getpad.dev", "HTTPS://App.GetPad.dev"},
		{"https://app.getpad.dev", "https://app.getpad.dev:443"},
		{"http://192.168.1.89:7777", "http://192.168.1.89:7777/"},
		{"http://pad.local", "http://pad.local:80"},
		{"https://pad.example.com/pad", "https://pad.example.com/pad/"},
	}
	for _, c := range same {
		if !sameServerURL(c[0], c[1]) {
			t.Errorf("sameServerURL(%q, %q) = false, want true", c[0], c[1])
		}
	}
	different := [][2]string{
		{"", "https://app.getpad.dev"},
		{"https://app.getpad.dev", "http://app.getpad.dev"},
		{"https://app.getpad.dev", "https://app.getpad.dev:8443"},
		{"http://192.168.1.89:7777", "http://192.168.1.89:7778"},
		{"https://pad.example.com/a", "https://pad.example.com/b"},
		{"https://pad.example.com", "https://other.example.com"},
		{"https://pad.example.com/a%2Fb", "https://pad.example.com/a/b"},
		{"https://pad.example.com/a", "https://pad.example.com/a//"},
	}
	for _, c := range different {
		if sameServerURL(c[0], c[1]) {
			t.Errorf("sameServerURL(%q, %q) = true, want false", c[0], c[1])
		}
	}
}
