package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func runAgendaCLI(t *testing.T, h http.HandlerFunc, args ...string) (out, stderr string, err error) {
	t.Helper()
	root := tzCLIWithLocal(t, h, time.UTC)
	root.SetArgs(append([]string{"item", "remind", "agenda"}, args...))
	stderr = captureStderr(t, func() { out = captureStdout(t, func() { err = root.Execute() }) })
	return out, stderr, err
}

func TestRemindAgendaAllWorkspacesHitsMe(t *testing.T) {
	t.Setenv("PAD_AGENT", "")
	t.Setenv("CLAUDECODE", "")

	body := `{
		"from":"2031-03-10T00:00:00Z",
		"to":"2031-03-17T00:00:00Z",
		"tz":"America/New_York",
		"audience":"human",
		"window_complete":true,
		"entries":[
			{"type":"schedule","at":"2031-03-10T14:00:00Z","day":"2031-03-10","state":"scheduled","item_ref":"TASK-1","item_title":"Review","workspace":"alpha","roles":{}}
		],
		"priority_list":[
			{"type":"schedule","item_ref":"TASK-2","item_title":"Ship","collection":"tasks","rank":"high","reason":"outstanding","at":"2031-03-10T15:00:00Z","workspace":"beta","roles":{}}
		],
		"priority_list_complete":true,
		"waiting_on":[
			{"type":"item","item_ref":"TASK-3","item_title":"Blocked","collection":"tasks","reason":"waiting","workspace":"gamma"}
		],
		"waiting_on_complete":true,
		"no_date":[
			{"type":"item","item_ref":"TASK-4","item_title":"Unplaced","collection":"tasks","reason":"unplaced","workspace":"delta"}
		],
		"no_date_complete":true
	}`
	h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, body })

	out, stderr, err := runAgendaCLI(t, h, "--from", "2031-03-10", "--all-workspaces", "--audience", "human")
	if err != nil {
		t.Fatalf("err=%v stderr=%q", err, stderr)
	}
	var hit *recordedCall
	for i := range calls() {
		c := calls()[i]
		if c.method == "GET" && c.path == "/me/agenda" {
			hit = &c
			break
		}
	}
	if hit == nil {
		t.Fatalf("expected GET /me/agenda call, got %+v", calls())
	}
	q, _ := url.ParseQuery(hit.query)
	if q.Get("audience") != "human" {
		t.Errorf("audience = %q, want human (query=%s)", q.Get("audience"), hit.query)
	}
	if !q.Has("include") || q.Get("include") != "" {
		t.Errorf("include = %q (has=%v), want empty include= (query=%s)", q.Get("include"), q.Has("include"), hit.query)
	}
	if q.Get("counts") != "omit" {
		t.Errorf("counts = %q, want omit (query=%s)", q.Get("counts"), hit.query)
	}
	if !strings.Contains(stderr, "Audience: human") {
		t.Errorf("stderr %q missing Audience: human", stderr)
	}
	for _, want := range []string{"WORKSPACE", "alpha", "beta", "gamma", "delta"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}

	t.Run("401 says to drop --all-workspaces", func(t *testing.T) {
		unauth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/me/agenda") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"code":"unauthorized","message":"Sign in to see your agenda"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"user-viewer","name":"Viewer","timezone":"UTC"}`)
		})
		_, _, err := runAgendaCLI(t, unauth, "--from", "2031-03-10", "--all-workspaces")
		if err == nil || !strings.Contains(err.Error(), "--all-workspaces") {
			t.Fatalf("401 err=%v, want guidance to drop --all-workspaces", err)
		}
	})
}

func TestRemindAgendaDefaultAudienceByAgentName(t *testing.T) {
	t.Run("PAD_AGENT set sends agent", func(t *testing.T) {
		t.Setenv("PAD_AGENT", "tern")
		t.Setenv("CLAUDECODE", "")
		h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, agendaEmptyWindow + `}` })
		_, stderr, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), "item", "remind", "agenda", "--from", "2031-03-10")
		if err != nil {
			t.Fatal(err)
		}
		q := agendaCall(t, calls())
		if q.Get("audience") != "agent" {
			t.Fatalf("query=%v, want audience=agent", q)
		}
		if !strings.Contains(stderr, "Audience: agent (--audience human for the person's web view)") {
			t.Fatalf("stderr = %q, want agent hint", stderr)
		}
	})

	t.Run("PAD_AGENT cleared sends human", func(t *testing.T) {
		t.Setenv("PAD_AGENT", "")
		t.Setenv("CLAUDECODE", "")
		h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, agendaEmptyWindow + `}` })
		_, stderr, err := tzRun(t, tzCLIWithLocal(t, h, time.UTC), "item", "remind", "agenda", "--from", "2031-03-10")
		if err != nil {
			t.Fatal(err)
		}
		q := agendaCall(t, calls())
		if q.Get("audience") != "human" {
			t.Fatalf("query=%v, want audience=human", q)
		}
		if !strings.Contains(stderr, "Audience: human") || strings.Contains(stderr, "--audience human") {
			t.Fatalf("stderr = %q, want Audience: human without agent hint", stderr)
		}
	})
}

func TestRemindAgendaAllWorkspacesRefusals(t *testing.T) {
	t.Setenv("PAD_AGENT", "")
	t.Setenv("CLAUDECODE", "")

	cases := map[string][]string{
		"stale":     {"--all-workspaces", "--stale"},
		"recipient": {"--all-workspaces", "--recipient", "all"},
		"workspace": {"--workspace", "test", "--all-workspaces"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, agendaEmptyWindow + `}` })
			_, _, err := runAgendaCLI(t, h, args...)
			if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
				t.Fatalf("expected combination refusal for %v, got err=%v", args, err)
			}
			for _, c := range calls() {
				if strings.Contains(c.path, "agenda") {
					t.Fatalf("%v reached server: %+v", args, calls())
				}
			}
		})
	}
}

func TestRemindAgendaResumeHintCarriesFlags(t *testing.T) {
	t.Setenv("PAD_AGENT", "")
	t.Setenv("CLAUDECODE", "")

	body := `{"from":"x","to":"y","tz":"UTC","entries":[{"type":"schedule","at":"2031-03-10T09:00:00Z","day":"2031-03-10","state":"scheduled","item_ref":"TASK-1","item_title":"A","workspace":"alpha","roles":{}}],"truncated":true,"next_from":"2031-03-11T00:00:00Z","priority_list":[],"priority_list_complete":true,"waiting_on":[],"waiting_on_complete":true,"no_date":[],"no_date_complete":true}`
	h, _ := recordingStub(func(c recordedCall) (int, string) { return 200, body })

	out, _, err := runAgendaCLI(t, h, "--from", "2031-03-10", "--all-workspaces", "--audience", "human")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--all-workspaces --audience human") {
		t.Fatalf("resume hint missing --all-workspaces --audience human:\n%s", out)
	}
}

func TestRemindAgendaStaleRefusesAudience(t *testing.T) {
	t.Setenv("PAD_AGENT", "")
	t.Setenv("CLAUDECODE", "")
	h, calls := recordingStub(func(c recordedCall) (int, string) { return 200, agendaEmptyWindow + `}` })
	_, _, err := runAgendaCLI(t, h, "--stale", "--audience", "bogus")
	if err == nil || !strings.Contains(err.Error(), "drop --audience") {
		t.Fatalf("expected --stale --audience refusal, got err=%v", err)
	}
	for _, c := range calls() {
		if strings.Contains(c.path, "agenda") {
			t.Fatalf("refusal reached server: %+v", calls())
		}
	}
}
