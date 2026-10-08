package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TASK-3462 U3c: `pad library diff` and `pad library update`, driven through
// the real commands against a fake server.

// u3cFormat is the --format the next u3cRun uses.
var u3cFormat = "table"

type u3cFake struct {
	caps      bool
	capsCode  int
	item      map[string]any
	postWarn  map[string]any
	state     map[string]any
	getCode   int
	getBody   map[string]any
	postCode  int
	postBody  map[string]any
	posted    []map[string]any
	gotStates int
}

func (f *u3cFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/server/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if f.capsCode != 0 {
			w.WriteHeader(f.capsCode)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"builtin_update": f.caps, "library_activate": true})
	})
	mux.HandleFunc("/api/v1/workspaces/ws/items/plan/builtin", func(w http.ResponseWriter, r *http.Request) {
		f.gotStates++
		w.Header().Set("Content-Type", "application/json")
		if f.getCode != 0 {
			w.WriteHeader(f.getCode)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": f.getBody})
			return
		}
		_ = json.NewEncoder(w).Encode(f.state)
	})
	mux.HandleFunc("/api/v1/workspaces/ws/items/plan", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.item)
	})
	mux.HandleFunc("/api/v1/workspaces/ws/items/plan/builtin/update", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.posted = append(f.posted, body)
		w.Header().Set("Content-Type", "application/json")
		code := f.postCode
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		if code >= 400 {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": f.postBody})
			return
		}
		resp := map[string]any{"id": "i1", "slug": "plan", "title": "Plan a new initiative", "seq": 9}
		if f.postWarn != nil {
			resp["warnings"] = f.postWarn
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func u3cRun(t *testing.T, srv *httptest.Server, build func() interface {
	SetArgs([]string)
	Execute() error
}, args ...string) (string, error) {
	t.Helper()
	setTempHomeMain(t)
	t.Setenv("PAD_URL", srv.URL)
	t.Setenv("PAD_TOKEN", "pad_testtoken")
	origWS, origFormat := workspaceFlag, formatFlag
	t.Cleanup(func() { workspaceFlag, formatFlag = origWS, origFormat })
	workspaceFlag, formatFlag = "ws", u3cFormat
	cmd := build()
	cmd.SetArgs(args)
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	return out, err
}

func divergedState() map[string]any {
	return map[string]any{
		"key": "playbook/plan", "kind": "playbook", "state": "diverged", "seq": 7,
		"library": map[string]any{"content": "step one\nstep two, improved\n", "fields": map[string]any{"trigger": "manual"}},
		"seed":    map[string]any{"content": "step one\nstep two\n", "fields": map[string]any{"trigger": "manual"}},
		"current": map[string]any{"content": "step one\nstep two\nmy own step\n", "fields": map[string]any{"trigger": "on-release"}},
	}
}

func TestTASK3462U3c_DiffShowsBothChangesAndTheFields(t *testing.T) {
	f := &u3cFake{caps: true, state: divergedState()}
	out, err := u3cRun(t, f.server(t), func() interface {
		SetArgs([]string)
		Execute() error
	} {
		return libraryDiffCmd()
	}, "plan")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	for _, want := range []string{
		"Library changed",
		"What Pad's library changed",
		"+step two, improved",
		"What you changed",
		"+my own step",
		"trigger: on-release → manual",
		"pad library update plan",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output lacks %q:\n%s", want, out)
		}
	}
	if len(f.posted) != 0 {
		t.Fatal("diff wrote")
	}
}

func TestTASK3462U3c_DiffOfAnItemMadeFromNoBuiltin(t *testing.T) {
	f := &u3cFake{caps: true, getCode: http.StatusNotFound, getBody: map[string]any{"code": "not_builtin", "message": "not built-in"}}
	_, err := u3cRun(t, f.server(t), func() interface {
		SetArgs([]string)
		Execute() error
	} {
		return libraryDiffCmd()
	}, "plan")
	if err == nil || !strings.Contains(err.Error(), "not made from a convention or playbook Pad ships") {
		t.Fatalf("want the not-built-in error, got %v", err)
	}
}

func TestTASK3462U3c_UpdateSendsTheSeqItRead(t *testing.T) {
	f := &u3cFake{caps: true, state: divergedState()}
	out, err := u3cRun(t, f.server(t), func() interface {
		SetArgs([]string)
		Execute() error
	} {
		return libraryUpdateCmd()
	}, "plan")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(f.posted) != 1 || f.posted[0]["expected_seq"] != float64(7) {
		t.Fatalf("posted %v, want expected_seq 7", f.posted)
	}
	if _, has := f.posted[0]["overwrite_pending_edits"]; has {
		t.Fatalf("sent overwrite_pending_edits without the flag: %v", f.posted[0])
	}
	if !strings.Contains(out, "Updated") {
		t.Errorf("no confirmation: %q", out)
	}
}

func TestTASK3462U3c_UpdateRefusesAnOlderServer(t *testing.T) {
	f := &u3cFake{caps: false, state: divergedState()}
	_, err := u3cRun(t, f.server(t), func() interface {
		SetArgs([]string)
		Execute() error
	} {
		return libraryUpdateCmd()
	}, "plan")
	if err == nil || !strings.Contains(err.Error(), "older than this CLI") {
		t.Fatalf("want the skew refusal, got %v", err)
	}
	if len(f.posted) != 0 || f.gotStates != 0 {
		t.Fatal("an older server was sent the request anyway")
	}
}

func TestTASK3462U3c_UpdateOfACurrentItemSendsNothing(t *testing.T) {
	f := &u3cFake{caps: true, state: map[string]any{"key": "playbook/plan", "state": "current", "seq": 7}}
	out, err := u3cRun(t, f.server(t), func() interface {
		SetArgs([]string)
		Execute() error
	} {
		return libraryUpdateCmd()
	}, "plan")
	if err != nil || !strings.Contains(out, "already") {
		t.Fatalf("want 'already up to date', got err=%v out=%q", err, out)
	}
	if len(f.posted) != 0 {
		t.Fatal("a current item was updated")
	}
}

func TestTASK3462U3c_PendingEditsNameTheFlagAndTheFlagSendsIt(t *testing.T) {
	f := &u3cFake{caps: true, state: divergedState(), postCode: http.StatusConflict,
		postBody: map[string]any{"code": "content_pending_flush", "message": "unflushed edits"}}
	_, err := u3cRun(t, f.server(t), func() interface {
		SetArgs([]string)
		Execute() error
	} {
		return libraryUpdateCmd()
	}, "plan")
	if err == nil || !strings.Contains(err.Error(), "--overwrite-pending-edits") {
		t.Fatalf("want the flag named, got %v", err)
	}
	f2 := &u3cFake{caps: true, state: divergedState()}
	if _, err := u3cRun(t, f2.server(t), func() interface {
		SetArgs([]string)
		Execute() error
	} {
		return libraryUpdateCmd()
	}, "plan", "--overwrite-pending-edits"); err != nil {
		t.Fatalf("update with the flag: %v", err)
	}
	if f2.posted[0]["overwrite_pending_edits"] != true {
		t.Fatalf("the flag did not send overwrite_pending_edits: %v", f2.posted[0])
	}
}

type u3cCmd = interface {
	SetArgs([]string)
	Execute() error
}

func withFormat(t *testing.T, f string) {
	t.Helper()
	u3cFormat = f
	t.Cleanup(func() { u3cFormat = "table" })
}

// codex r1 (P1): the JSON diff carries the current text even from a server
// that does not send it, read from the item itself.
func TestTASK3462U3c_JSONDiffCarriesCurrentFromAnOlderServer(t *testing.T) {
	withFormat(t, "json")
	st := divergedState()
	delete(st, "current")
	f := &u3cFake{caps: true, state: st, item: map[string]any{"id": "i1", "slug": "plan", "seq": 7, "content": "the item's own body", "fields": `{"trigger":"on-release"}`}}
	out, err := u3cRun(t, f.server(t), func() u3cCmd { return libraryDiffCmd() }, "plan")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	var got map[string]any
	if json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("not JSON: %q", out)
	}
	cur, _ := got["current"].(map[string]any)
	if cur == nil || cur["content"] != "the item's own body" {
		t.Fatalf("JSON diff lacks the current text: %v", got["current"])
	}
}

// codex r1 (P2): a no-op under --format json is JSON.
func TestTASK3462U3c_JSONNoOpIsJSON(t *testing.T) {
	withFormat(t, "json")
	f := &u3cFake{caps: true, state: map[string]any{"key": "playbook/plan", "state": "current", "seq": 7}}
	out, err := u3cRun(t, f.server(t), func() u3cCmd { return libraryUpdateCmd() }, "plan")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var got map[string]any
	if json.Unmarshal([]byte(out), &got) != nil || got["updated"] != false {
		t.Fatalf("a no-op under --format json is not a JSON result: %q", out)
	}
}

// codex r1 (P2): a probe that failed is not "the server is older".
func TestTASK3462U3c_IndeterminateProbeIsNotCalledSkew(t *testing.T) {
	f := &u3cFake{capsCode: http.StatusInternalServerError, state: divergedState()}
	_, err := u3cRun(t, f.server(t), func() u3cCmd { return libraryUpdateCmd() }, "plan")
	if err == nil || strings.Contains(err.Error(), "older than this CLI") || !strings.Contains(err.Error(), "could not tell") {
		t.Fatalf("want the indeterminate-probe refusal, got %v", err)
	}
	if len(f.posted) != 0 {
		t.Fatal("sent anyway")
	}
}

// codex r1 (P2): the write's warnings reach the caller.
func TestTASK3462U3c_UpdateReportsWriteWarnings(t *testing.T) {
	f := &u3cFake{caps: true, state: divergedState(), postWarn: map[string]any{"content_outcome": "applied_pending_flush", "pruned_pending_edits": 2}}
	var out string
	stderr := captureStderr(t, func() {
		out, _ = u3cRun(t, f.server(t), func() u3cCmd { return libraryUpdateCmd() }, "plan")
	})
	_ = out
	if !strings.Contains(stderr, "live collaborative document") || !strings.Contains(stderr, "replaced 2 unflushed edit") {
		t.Fatalf("warnings not reported: %q", stderr)
	}
}

// codex r1 (P2): set-aside rows get their own remedy, not "an open editor".
func TestTASK3462U3c_SetAsideRowsAreNamedAsSuch(t *testing.T) {
	f := &u3cFake{caps: true, state: divergedState(), postCode: http.StatusConflict,
		postBody: map[string]any{"code": "content_pending_flush", "message": "set aside", "details": map[string]any{"set_aside_rows": 3}}}
	var err error
	stderr := captureStderr(t, func() {
		_, err = u3cRun(t, f.server(t), func() u3cCmd { return libraryUpdateCmd() }, "plan")
	})
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error()+stderr, "open editor has not saved") || !strings.Contains(stderr, "discard those edits") {
		t.Fatalf("set-aside refusal misdescribed: err=%v stderr=%q", err, stderr)
	}
}

// codex r2 (P1): numbers keep their digits, so two integers above 2^53 that a
// float64 would merge still show as a change.
func TestTASK3462U3c_LargeNumbersAreNotRounded(t *testing.T) {
	st := divergedState()
	st["library"].(map[string]any)["fields"] = map[string]any{"limit": json.Number("9007199254740993")}
	st["current"].(map[string]any)["fields"] = map[string]any{"limit": json.Number("9007199254740992")}
	f := &u3cFake{caps: true, state: st}
	out, err := u3cRun(t, f.server(t), func() u3cCmd { return libraryDiffCmd() }, "plan")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(out, "limit: 9007199254740992 → 9007199254740993") {
		t.Fatalf("a large-number change was rounded away:\n%s", out)
	}
}

// codex r2 (P2): a fallback read of a newer version than the state described
// is refused, not mixed in.
func TestTASK3462U3c_FallbackFromAnotherVersionIsRefused(t *testing.T) {
	st := divergedState()
	delete(st, "current")
	f := &u3cFake{caps: true, state: st, item: map[string]any{"id": "i1", "slug": "plan", "seq": 8, "content": "newer", "fields": `{}`}}
	_, err := u3cRun(t, f.server(t), func() u3cCmd { return libraryDiffCmd() }, "plan")
	if err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Fatalf("want the version-mismatch refusal, got %v", err)
	}
}
