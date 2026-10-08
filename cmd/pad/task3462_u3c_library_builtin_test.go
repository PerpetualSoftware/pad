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

type u3cFake struct {
	caps      bool
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
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "i1", "slug": "plan", "title": "Plan a new initiative", "seq": 9})
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
	workspaceFlag, formatFlag = "ws", "table"
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
