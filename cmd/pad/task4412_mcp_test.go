package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestTASK4412_MCPTransportParity(t *testing.T) {
	cases := []struct {
		action       string
		in           map[string]any
		method, path string
	}{
		{"schedule-link", map[string]any{"ref": "TASK-5", "child": "TASK-6", "parent_role": "target", "child_role": "earliest", "link_offset": map[string]any{"mode": "elapsed", "elapsed_secs": -43200}}, "POST", schedPath + "/links"},
		{"schedule-link", map[string]any{"link_id": "lid", "link_offset": map[string]any{"mode": "civil", "days": -1, "clock_secs": 68400, "all_day": false, "zone": "America/New_York"}, "apply": true, "revision": "X", "acknowledge_violations": true}, "PATCH", "/workspaces/test/commitment-links/lid"},
		{"schedule-unlink", map[string]any{"link_id": "lid"}, "DELETE", "/workspaces/test/commitment-links/lid"},
		{"schedule-move", map[string]any{"ref": "TASK-5", "role": "target", "to": "2099-01-02T09:00:00Z"}, "POST", schedPath + "/move-preview"},
		{"schedule-move", map[string]any{"ref": "TASK-5", "role": "target", "to": "2099-01-02T09:00:00+02:00", "apply": true, "revision": "X", "reason": "why", "acknowledge_violations": true}, "POST", schedPath + "/move"},
		{"schedule-gate", map[string]any{"ref": "TASK-5", "blocker": "TASK-6"}, "POST", schedPath + "/gates"},
		{"schedule-gate", map[string]any{"gate_id": "gid"}, "DELETE", "/workspaces/test/commitment-gates/gid"},
	}
	for _, tc := range cases {
		for _, code := range []string{"", "schedule_link_not_found", "commitment_changed", "link_violation_ack", "invalid_body", "validation_error"} {
			t.Run(tc.action+tc.method+code, func(t *testing.T) {
				var fn mcp.ActionFn
				for _, def := range mcp.Catalog {
					if def.Name == "pad_item" {
						fn = def.Actions[tc.action]
					}
				}
				if fn == nil {
					t.Fatalf("missing action %s", tc.action)
				}
				reply := func(recordedCall) (int, string) {
					if code != "" {
						status := 409
						if code == "invalid_body" || code == "validation_error" {
							status = 400
						}
						if code == "schedule_link_not_found" {
							status = 404
						}
						return status, `{"error":{"code":"` + code + `","message":"Refused","details":{"field":"revision","code":"stale"}}}`
					}
					if tc.method == "DELETE" {
						return 204, ""
					}
					return 200, linkPlan
				}
				h, get := recordingStub(reply)
				root := tzCLIWithLocal(t, h, time.UTC)
				t.Setenv(padHelperEnv, "1")
				bin, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
				input := map[string]any{"workspace": "test"}
				for k, v := range tc.in {
					input[k] = v
				}
				env := mcp.ActionEnv{Doc: doc, Workspace: mcp.NewWorkspaceState("test"), Dispatcher: &mcp.ExecDispatcher{Binary: bin}}
				stdio, err := fn(context.Background(), input, env)
				if err != nil {
					t.Fatal(err)
				}
				cliCalls := get()
				if len(cliCalls) != 1 {
					t.Fatalf("stdio: %+v calls=%+v", stdio, cliCalls)
				}
				var remoteCalls []recordedCall
				rh, _ := recordingStub(func(c recordedCall) (int, string) { remoteCalls = append(remoteCalls, c); return reply(c) })
				env.Dispatcher = &mcp.HTTPHandlerDispatcher{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Body == nil {
						r.Body = http.NoBody
					}
					rh(w, r)
				}), UserResolver: func(context.Context) *models.User { return &models.User{ID: "u"} }}
				remote, err := fn(context.Background(), input, env)
				if err != nil {
					t.Fatal(err)
				}
				if len(remoteCalls) != 1 {
					t.Fatalf("remote: %+v calls=%+v", remote, remoteCalls)
				}
				a, b := cliCalls[0], remoteCalls[0]
				var ab, bb any
				_ = json.Unmarshal([]byte(a.body), &ab)
				_ = json.Unmarshal([]byte(b.body), &bb)
				if a.method != tc.method || a.path != tc.path || a.method != b.method || a.path != b.path || !reflect.DeepEqual(ab, bb) {
					t.Fatalf("stdio=%+v remote=%+v", a, b)
				}
				if code != "" {
					for _, res := range []*mcp.CallToolResult{stdio, remote} {
						raw, _ := json.Marshal(res.StructuredContent)
						var v struct {
							Error struct {
								Code, Message string
								Details       map[string]any
							}
						}
						_ = json.Unmarshal(raw, &v)
						if !res.IsError || v.Error.Code != code || v.Error.Message != "Refused" || v.Error.Details["field"] != "revision" || v.Error.Details["code"] != "stale" {
							t.Fatalf("error=%s", raw)
						}
					}
				} else if stdio.IsError || remote.IsError {
					t.Fatalf("stdio=%+v remote=%+v", stdio, remote)
				}
				if code == "" && tc.method != "DELETE" && !strings.Contains(a.body, `"preview":true`) && !strings.Contains(tc.path, "move") && !strings.Contains(tc.path, "gates") && input["apply"] != true {
					t.Errorf("missing preview: %s", a.body)
				}
			})
		}
	}
}

func TestTASK4412_ScheduleOwnedInputs(t *testing.T) {
	const occurrence = "2099-01-01T09:00:00Z"
	const until = "2099-01-02T09:00:00Z"
	cases := []struct {
		name, action       string
		input, stray       map[string]any
		method, path, body string
	}{
		{"link add", "schedule-link", map[string]any{"ref": "TASK-5", "child": "TASK-6", "parent_role": "target", "child_role": "earliest", "link_offset": map[string]any{"mode": "elapsed", "elapsed_secs": -43200}}, map[string]any{"offset": 20, "days": 3, "comment": "unrelated", "until": until, "role": "implementer"}, "POST", schedPath + "/links", `{"child":"TASK-6","parent_role":"target","child_role":"earliest","offset":{"mode":"elapsed","elapsed_secs":-43200},"preview":true}`},
		{"link edit", "schedule-link", map[string]any{"link_id": "lid", "link_offset": map[string]any{"mode": "elapsed", "elapsed_secs": 0}}, map[string]any{"offset": 20, "days": 3, "comment": "unrelated", "until": until}, "PATCH", "/workspaces/test/commitment-links/lid", `{"offset":{"mode":"elapsed","elapsed_secs":0},"preview":true}`},
		{"move preview", "schedule-move", map[string]any{"ref": "TASK-5", "role": "target", "to": until}, map[string]any{"until": until, "comment": "unrelated", "offset": 20, "days": 3}, "POST", schedPath + "/move-preview", `{"role":"target","to":"2099-01-02T09:00:00Z"}`},
		{"move apply", "schedule-move", map[string]any{"ref": "TASK-5", "role": "target", "to": until, "apply": true, "revision": "X"}, map[string]any{"until": until}, "POST", schedPath + "/move", `{"role":"target","to":"2099-01-02T09:00:00Z","revision":"X"}`},
		{"unlink", "schedule-unlink", map[string]any{"link_id": "lid"}, map[string]any{"apply": "unrelated", "reason": 20}, "DELETE", "/workspaces/test/commitment-links/lid", ""},
		{"gate add", "schedule-gate", map[string]any{"ref": "TASK-5", "blocker": "TASK-6"}, map[string]any{"apply": "unrelated", "reason": 20}, "POST", schedPath + "/gates", `{"blocker":"TASK-6"}`},
		{"gate remove", "schedule-gate", map[string]any{"gate_id": "gid"}, map[string]any{"apply": "unrelated", "reason": 20}, "DELETE", "/workspaces/test/commitment-gates/gid", ""},
	}
	for _, state := range []string{"done", "skip", "snooze", "move"} {
		input := map[string]any{"ref": "TASK-5", "state": state, "occurrence_at": occurrence, "comment": "resolve comment"}
		body := map[string]any{"state": state, "occurrence_at": occurrence, "comment": "resolve comment"}
		if state == "snooze" || state == "move" {
			input["until"], body["until"] = until, until
		}
		if state == "done" {
			input["actual_done_at"], body["actual_done_at"] = occurrence, occurrence
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"role", "to"} {
			cases = append(cases, struct {
				name, action       string
				input, stray       map[string]any
				method, path, body string
			}{"resolve " + state + " with " + key, "schedule-resolve", input, map[string]any{key: map[string]string{"role": "implementer", "to": until}[key]}, "POST", schedPath + "/resolve", string(raw)})
		}
	}
	for _, tc := range cases {
		for _, transport := range []string{"stdio", "remote"} {
			t.Run(tc.name+"/"+transport, func(t *testing.T) {
				h, calls := recordingStub(func(recordedCall) (int, string) {
					if tc.method == "DELETE" {
						return 204, ""
					}
					return 200, `{}`
				})
				root := tzCLIWithLocal(t, h, time.UTC)
				t.Setenv(padHelperEnv, "1")
				bin, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				env := mcp.ActionEnv{Doc: cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1}), Workspace: mcp.NewWorkspaceState("test"), Dispatcher: &mcp.ExecDispatcher{Binary: bin}}
				if transport == "remote" {
					env.Dispatcher = &mcp.HTTPHandlerDispatcher{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Body == nil {
							r.Body = http.NoBody
						}
						h(w, r)
					}), UserResolver: func(context.Context) *models.User { return &models.User{ID: "u"} }}
				}
				var action mcp.ActionFn
				for _, def := range mcp.Catalog {
					if def.Name == "pad_item" {
						action = def.Actions[tc.action]
					}
				}
				if action == nil {
					t.Fatalf("missing action %s", tc.action)
				}
				// Both the clean and padded inputs must produce the pinned request.
				for _, padded := range []bool{false, true} {
					input := map[string]any{"workspace": "test"}
					for k, v := range tc.input {
						input[k] = v
					}
					if padded {
						for k, v := range tc.stray {
							input[k] = v
						}
					}
					before := len(calls())
					res, err := action(context.Background(), input, env)
					if err != nil || res == nil || res.IsError {
						t.Errorf("padded=%t: err=%v result=%+v", padded, err, res)
					}
					got := calls()[before:]
					if len(got) != 1 {
						t.Errorf("padded=%t: calls=%+v; want exactly one %s %s", padded, got, tc.method, tc.path)
						continue
					}
					var body, want any
					_ = json.Unmarshal([]byte(got[0].body), &body)
					_ = json.Unmarshal([]byte(tc.body), &want)
					if got[0].method != tc.method || got[0].path != tc.path || got[0].query != "" || !reflect.DeepEqual(body, want) {
						t.Errorf("padded=%t: got %+v; want %s %s %s", padded, got[0], tc.method, tc.path, tc.body)
					}
				}
			})
		}
	}
}

func TestTASK4412_GraphBodyErrors(t *testing.T) {
	setTempHomeMain(t)
	for _, code := range []string{"invalid_body", "validation_error"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"Invalid offset","details":{"field":"offset","code":"combination"}}}`))
		}))
		t.Cleanup(srv.Close)
		client := cli.NewClientFromURLWithAgentName(srv.URL, "")
		client.SetHTTPClient(srv.Client())
		_, err := client.AddCommitmentLink("test", "TASK-5", map[string]any{})
		var marker strings.Builder
		writeRootStructuredError(&marker, err)
		want := cli.StructuredErrorMarker + `{"error":{"code":"` + code + `","message":"Invalid offset","details":{"field":"offset","code":"combination"}}}` + "\n"
		if marker.String() != want {
			t.Errorf("%s: marker=%q, want %q", code, marker.String(), want)
		}
	}
}

func TestTASK4412_NonCommitmentInvalidBody(t *testing.T) {
	const body = `{"error":{"code":"invalid_body","message":"Invalid JSON body"}}`
	h, calls := recordingStub(func(recordedCall) (int, string) { return 400, body })
	tzCLIWithLocal(t, h, time.UTC)
	t.Setenv(padHelperEnv, "1")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmdPath := []string{"item", "move"}
	argv := []string{"TASK-5", "tasks", "--workspace", "test", "--format", "json"}
	// Pin main's CLI bytes, including the absence of a structured marker.
	t.Run("CLI JSON", func(t *testing.T) {
		cmd := exec.Command(bin, append(append([]string{}, cmdPath...), argv...)...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		var exit *exec.ExitError
		if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("exit=%v", err)
		}
		if stdout.String() != "" || stderr.String() != "Error: Invalid JSON body\n" {
			t.Errorf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	})
	dispatchers := []struct {
		name string
		d    mcp.Dispatcher
		hint string
	}{
		{"stdio", &mcp.ExecDispatcher{Binary: bin}, "Invalid JSON body"},
		{"HTTP", &mcp.HTTPHandlerDispatcher{Handler: h, UserResolver: func(context.Context) *models.User { return &models.User{ID: "u"} }}, "Backend: Invalid JSON body Adjust the input shape and retry."},
	}
	for _, tc := range dispatchers {
		t.Run(tc.name, func(t *testing.T) {
			ctx := mcp.WithDispatchInput(context.Background(), map[string]any{"workspace": "test", "ref": "TASK-5", "target_collection": "tasks"})
			res, err := tc.d.Dispatch(ctx, cmdPath, argv)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(mcp.ErrorEnvelope{Error: mcp.ErrorPayload{Code: mcp.ErrValidationFailed, Message: "Validation failed.", Hint: tc.hint}})
			if !res.IsError || string(got) != string(want) {
				t.Errorf("got %s; want main's %s", got, want)
			}
		})
	}
	if got := calls(); len(got) != 3 {
		t.Fatalf("requests=%+v; want one move per surface", got)
	} else {
		for _, call := range got {
			if call.method != "POST" || call.path != "/workspaces/test/items/TASK-5/move" {
				t.Errorf("unexpected request: %+v", call)
			}
		}
	}
}
