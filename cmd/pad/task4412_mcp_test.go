package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
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

func TestTASK4412_GraphBodyErrors(t *testing.T) {
	for _, code := range []string{"invalid_body", "validation_error"} {
		var marker strings.Builder
		writeRootStructuredError(&marker, &cli.APIError{Status: 400, Code: code, Message: "Invalid offset", Details: json.RawMessage(`{"field":"offset","code":"combination"}`)})
		if !strings.Contains(marker.String(), `"code":"`+code+`"`) {
			t.Errorf("%s: missing structured marker: %s", code, marker.String())
		}
	}
}
