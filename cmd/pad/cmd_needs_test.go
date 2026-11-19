package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// needsFake records every request (method, path, query and decoded body)
// and answers from a per-route table. An unrouted request fails the test.
type needsFake struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []string
	bodies map[string]map[string]any
	query  map[string]string
	routes map[string]func(w http.ResponseWriter, body map[string]any)
}

func newNeedsFake(t *testing.T) *needsFake {
	return &needsFake{t: t, bodies: map[string]map[string]any{}, query: map[string]string{},
		routes: map[string]func(http.ResponseWriter, map[string]any){}}
}

func (f *needsFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Errorf("%s: body %q is not a JSON object: %v", key, raw, err)
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.bodies[key] = body
	f.query[key] = r.URL.RawQuery
	route := f.routes[key]
	f.mu.Unlock()
	if route == nil {
		f.t.Errorf("unexpected request %s", key)
		http.NotFound(w, r)
		return
	}
	route(w, body)
}

func (f *needsFake) on(key, response string) {
	f.routes[key] = func(w http.ResponseWriter, _ map[string]any) { fmt.Fprint(w, response) }
}

func runNeedsCmd(t *testing.T, f *needsFake, args ...string) (string, error) {
	t.Helper()
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })
	ts := setupPushTest(t, f)
	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{"--url", ts.URL}, args...))
	err := cmd.Execute()
	return out.String(), err
}

const needsItemPath = "/api/v1/workspaces/demo/items/"

func TestNeedsNotMine_ToRoleSendsPatchThenPut(t *testing.T) {
	f := newNeedsFake(t)
	f.on("GET /api/v1/workspaces/demo/agent-roles/reviewer", `{"id":"role-1","slug":"reviewer","name":"Reviewer"}`)
	for i, ref := range []string{"TASK-1", "TASK-2"} {
		seq := 7 + i
		f.on("GET "+needsItemPath+ref, fmt.Sprintf(`{"id":"i%d","ref":%q,"seq":%d,"title":"t"}`, i, ref, seq))
		f.on("PATCH "+needsItemPath+ref, fmt.Sprintf(`{"id":"i%d","ref":%q,"seq":%d,"title":"t"}`, i, ref, seq+1))
		f.on("PUT "+needsItemPath+ref+"/needs/not-mine", fmt.Sprintf(`{"ref":%q,"not_mine":{"source":"cli"}}`, ref))
	}
	out, err := runNeedsCmd(t, f, "--workspace", "demo", "needs", "not-mine", "TASK-1", "TASK-2", "--to-role", "reviewer")
	if err != nil {
		t.Fatalf("not-mine: %v", err)
	}
	want := []string{
		"GET /api/v1/workspaces/demo/agent-roles/reviewer",
		"GET " + needsItemPath + "TASK-1", "PATCH " + needsItemPath + "TASK-1", "PUT " + needsItemPath + "TASK-1/needs/not-mine",
		"GET " + needsItemPath + "TASK-2", "PATCH " + needsItemPath + "TASK-2", "PUT " + needsItemPath + "TASK-2/needs/not-mine",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	for i, ref := range []string{"TASK-1", "TASK-2"} {
		patch := f.bodies["PATCH "+needsItemPath+ref]
		if patch["agent_role_id"] != "role-1" || patch["clear_assigned_user"] != true || patch["expected_seq"] != float64(7+i) {
			t.Errorf("%s patch body: %v", ref, patch)
		}
		if _, ok := patch["assigned_user_id"]; ok {
			t.Errorf("%s patch sets and clears the person: %v", ref, patch)
		}
		put := f.bodies["PUT "+needsItemPath+ref+"/needs/not-mine"]
		if put == nil || len(put) != 0 {
			t.Errorf("%s PUT body = %v, want {} (no marker, no source)", ref, put)
		}
	}
	if !strings.Contains(out, "TASK-1 given to Reviewer and marked not mine") {
		t.Errorf("output: %q", out)
	}
}

func TestNeedsNotMine_PlainSendsOnlyPut(t *testing.T) {
	f := newNeedsFake(t)
	f.on("PUT "+needsItemPath+"TASK-1/needs/not-mine", `{"ref":"TASK-1","not_mine":{"source":"cli"}}`)
	out, err := runNeedsCmd(t, f, "--workspace", "demo", "needs", "not-mine", "TASK-1")
	if err != nil {
		t.Fatalf("not-mine: %v", err)
	}
	if strings.Join(f.calls, ",") != "PUT "+needsItemPath+"TASK-1/needs/not-mine" {
		t.Fatalf("calls: %v", f.calls)
	}
	if put := f.bodies["PUT "+needsItemPath+"TASK-1/needs/not-mine"]; put == nil || len(put) != 0 {
		t.Fatalf("PUT body = %v, want {}", put)
	}
	if !strings.Contains(out, "TASK-1 marked not mine") {
		t.Errorf("output: %q", out)
	}
}

func TestNeedsNotMine_ToUserResolvesMember(t *testing.T) {
	f := newNeedsFake(t)
	f.on("GET /api/v1/workspaces/demo/members", `{"members":[{"user_id":"u-1","user_name":"Bob","user_email":"bob@example.com"},{"user_id":"u-2","user_name":"Alice","user_email":"alice@example.com"}]}`)
	f.on("GET "+needsItemPath+"TASK-1", `{"id":"i1","ref":"TASK-1","seq":3}`)
	f.on("PATCH "+needsItemPath+"TASK-1", `{"id":"i1","ref":"TASK-1","seq":4}`)
	f.on("PUT "+needsItemPath+"TASK-1/needs/not-mine", `{"ref":"TASK-1","not_mine":{}}`)
	if _, err := runNeedsCmd(t, f, "--workspace", "demo", "needs", "not-mine", "TASK-1", "--to-user", "ALICE@example.com"); err != nil {
		t.Fatalf("not-mine: %v", err)
	}
	patch := f.bodies["PATCH "+needsItemPath+"TASK-1"]
	if patch["assigned_user_id"] != "u-2" || patch["expected_seq"] != float64(3) {
		t.Fatalf("patch body: %v", patch)
	}
	for _, k := range []string{"agent_role_id", "clear_assigned_user", "clear_agent_role"} {
		if _, ok := patch[k]; ok {
			t.Errorf("a person give touches %s: %v", k, patch)
		}
	}
	if f.calls[len(f.calls)-1] != "PUT "+needsItemPath+"TASK-1/needs/not-mine" {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestNeedsNotMine_UnknownUserWritesNothing(t *testing.T) {
	f := newNeedsFake(t)
	f.on("GET /api/v1/workspaces/demo/members", `{"members":[{"user_id":"u-1","user_name":"Bob"}]}`)
	_, err := runNeedsCmd(t, f, "--workspace", "demo", "needs", "not-mine", "TASK-1", "--to-user", "carol")
	if err == nil || !strings.Contains(err.Error(), "carol") {
		t.Fatalf("err = %v, want unknown member", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestNeedsNotMine_PatchConflictSkipsPut(t *testing.T) {
	f := newNeedsFake(t)
	f.on("GET /api/v1/workspaces/demo/agent-roles/reviewer", `{"id":"role-1","slug":"reviewer","name":"Reviewer"}`)
	f.on("GET "+needsItemPath+"TASK-1", `{"id":"i1","ref":"TASK-1","seq":7}`)
	f.routes["PATCH "+needsItemPath+"TASK-1"] = func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":{"code":"update_conflict","message":"item was modified","details":{"ref":"TASK-1","conflict_type":"seq","expected_seq":7,"actual_seq":8}}}`)
	}
	_, err := runNeedsCmd(t, f, "--workspace", "demo", "needs", "not-mine", "TASK-1", "TASK-2", "--to-role", "reviewer")
	if err == nil {
		t.Fatal("a refused reassignment must fail the command")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "PUT ") || strings.Contains(c, "TASK-2") {
			t.Fatalf("wrote past a refused PATCH: %v", f.calls)
		}
	}
}

func TestNeedsNotMine_FlagsExclusive(t *testing.T) {
	f := newNeedsFake(t)
	_, err := runNeedsCmd(t, f, "--workspace", "demo", "needs", "not-mine", "TASK-1", "--to-role", "reviewer", "--to-user", "alice")
	if err == nil || !strings.Contains(err.Error(), "none of the others can be") {
		t.Fatalf("err = %v, want mutually exclusive flags", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestNeedsRestore_DeletesBoth(t *testing.T) {
	f := newNeedsFake(t)
	for _, ref := range []string{"TASK-1", "TASK-2"} {
		f.on("DELETE "+needsItemPath+ref+"/needs/not-mine", fmt.Sprintf(`{"ref":%q,"not_mine":null}`, ref))
		f.on("DELETE "+needsItemPath+ref+"/needs/snooze", fmt.Sprintf(`{"ref":%q,"snooze":null}`, ref))
	}
	out, err := runNeedsCmd(t, f, "--workspace", "demo", "needs", "restore", "TASK-1", "TASK-2")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	want := []string{
		"DELETE " + needsItemPath + "TASK-1/needs/not-mine", "DELETE " + needsItemPath + "TASK-1/needs/snooze",
		"DELETE " + needsItemPath + "TASK-2/needs/not-mine", "DELETE " + needsItemPath + "TASK-2/needs/snooze",
	}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls: %v", f.calls)
	}
	if !strings.Contains(out, "TASK-2 restored") {
		t.Errorf("output: %q", out)
	}
}

func TestNeedsList_WorkspaceOnlyWhenExplicit(t *testing.T) {
	// A cwd link names a workspace; the inbox is cross-workspace, so only an
	// explicit --workspace narrows it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".pad.toml"), []byte("workspace = \"linked\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, tc := range []struct {
		name  string
		args  []string
		query string
	}{
		{"cwd link ignored", []string{"needs", "list"}, ""},
		{"explicit", []string{"--workspace", "demo", "needs", "list"}, "workspace=demo"},
		{"hidden", []string{"needs", "list", "--view", "hidden", "--limit", "5"}, "limit=5&view=hidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNeedsFake(t)
			f.on("GET /api/v1/me/needs", `{"rows":[],"total":0,"hidden":0}`)
			if _, err := runNeedsCmd(t, f, tc.args...); err != nil {
				t.Fatalf("list: %v", err)
			}
			if got := f.query["GET /api/v1/me/needs"]; got != tc.query {
				t.Fatalf("query = %q, want %q", got, tc.query)
			}
		})
	}
}

func TestNeedsList_HiddenShowsNotMineSince(t *testing.T) {
	f := newNeedsFake(t)
	f.on("GET /api/v1/me/needs", `{"rows":[
		{"workspace":{"slug":"docapp"},"item":{"ref":"TASK-1","title":"One"},"why":{"text":"You were mentioned"},"waiting_since":"2026-11-01T00:00:00Z","not_mine":{"created_at":"2026-11-18T10:00:00Z"}},
		{"workspace":{"slug":"docapp"},"item":{"ref":"TASK-2","title":"Two"},"why":{"text":"Assigned"},"waiting_since":"2026-11-01T00:00:00Z","snooze":{"kind":"dismiss"}},
		{"workspace":{"slug":"apm"},"item":{"ref":"BL-3","title":"Three"},"why":{"text":"Due"},"waiting_since":"2026-11-01T00:00:00Z","snooze":{"kind":"defer","defer_until":"2026-11-30T09:00:00Z"}}
	],"total":3,"hidden":3}`)
	f.on("GET /api/v1/auth/me", `{"timezone":"UTC"}`) // dates show in the viewer's zone
	out, err := runNeedsCmd(t, f, "needs", "list", "--view", "hidden")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"not mine since 2026-11-18", "dismissed", "deferred until 2026-11-30", "BL-3", "You were mentioned"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestNeedsRestore_StopNamesRefsDone(t *testing.T) {
	f := newNeedsFake(t)
	f.on("DELETE "+needsItemPath+"TASK-1/needs/not-mine", `{"ref":"TASK-1","not_mine":null}`)
	f.on("DELETE "+needsItemPath+"TASK-1/needs/snooze", `{"ref":"TASK-1","snooze":null}`)
	f.routes["DELETE "+needsItemPath+"TASK-2/needs/not-mine"] = func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"code":"internal_error","message":"boom"}}`)
	}
	_, err := runNeedsCmd(t, f, "--workspace", "demo", "needs", "restore", "TASK-1", "TASK-2", "TASK-3")
	if err == nil {
		t.Fatal("want an error for TASK-2")
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "TASK-2: ") || !strings.Contains(msg, "already done: TASK-1") {
		t.Fatalf("error = %q, want TASK-2 named and TASK-1 reported done", msg)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "TASK-3") {
			t.Fatalf("processing must stop at the first error; calls: %v", f.calls)
		}
	}
}
