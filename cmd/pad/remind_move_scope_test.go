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

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
)

const seriesPlan = `{"revision":"rev-1","changes":[{"item_id":"root","role":"target","from":"2099-01-01T09:00:00Z","to":"2099-01-01T14:00:00Z","root":true}],"series":{"applies_from":"2099-01-01T09:00:00Z|2099-01-01T09:00:00","current_offset":{"target_days":0,"target_secs":0},"new_offset":{"target_days":0,"target_secs":18000}}}`

func TestRemindMoveScopeForwarding(t *testing.T) {
	base := []string{"remind", "move", "TASK-5", "--role", "target", "--to", "2099-01-01T14:00:00Z", "--scope", "series"}
	out, _, err, calls := runSched(t, okReply(seriesPlan), "", base...)
	if err != nil || len(calls) != 1 || calls[0].path != schedPath+"/move-preview" || resolveBody(t, calls[0])["scope"] != "series" {
		t.Fatalf("preview err=%v calls=%+v", err, calls)
	}
	if !strings.Contains(out, "This and future occurrences") || !strings.Contains(out, "--scope series") {
		t.Fatalf("preview out=%q", out)
	}
	_, _, err, calls = runSched(t, okReply(seriesPlan), "", append(base, "--apply", "--revision", "rev-1")...)
	if err != nil || len(calls) != 1 || calls[0].path != schedPath+"/move" {
		t.Fatalf("apply err=%v calls=%+v", err, calls)
	}
	if b := resolveBody(t, calls[0]); b["scope"] != "series" || b["revision"] != "rev-1" {
		t.Fatalf("apply body=%s", calls[0].body)
	}
	// No scope: the body carries none.
	_, _, err, calls = runSched(t, okReply(linkPlan), "", "remind", "move", "TASK-5", "--role", "target", "--to", "2099-01-01T14:00:00Z")
	if _, ok := resolveBody(t, calls[0])["scope"]; err != nil || ok {
		t.Fatalf("default err=%v body=%s", err, calls[0].body)
	}
	// --scope belongs to the linked move form.
	_, _, err, calls = runSched(t, okReply(`{}`), "", "remind", "move", "TASK-5", "--until", "2099-01-01T14:00:00Z", "--scope", "series")
	if err == nil || !strings.Contains(err.Error(), "--scope requires --role") || len(calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, calls)
	}
}

func TestRemindMoveScopeMCPTransports(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]any
		path string
		code string
	}{
		{"preview", map[string]any{"ref": "TASK-5", "role": "target", "to": "2099-01-01T14:00:00Z", "scope": "series"}, schedPath + "/move-preview", ""},
		{"apply", map[string]any{"ref": "TASK-5", "role": "target", "to": "2099-01-01T14:00:00Z", "scope": "series", "apply": true, "revision": "rev-1"}, schedPath + "/move", ""},
		{"refusal", map[string]any{"ref": "TASK-5", "role": "earliest", "to": "2099-01-01T14:00:00Z", "scope": "series"}, schedPath + "/move-preview", "combination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fn mcp.ActionFn
			for _, def := range mcp.Catalog {
				if def.Name == "pad_item" {
					fn = def.Actions["schedule-move"]
				}
			}
			reply := func(recordedCall) (int, string) {
				if tc.code != "" {
					return 400, `{"error":{"code":"validation_error","message":"Invalid commitment scope","details":{"field":"scope","code":"` + tc.code + `"}}}`
				}
				return 200, seriesPlan
			}
			h, get := recordingStub(reply)
			root := tzCLIWithLocal(t, h, time.UTC)
			t.Setenv(padHelperEnv, "1")
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			input := map[string]any{"workspace": "test"}
			for k, v := range tc.in {
				input[k] = v
			}
			env := mcp.ActionEnv{Doc: cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1}), Workspace: mcp.NewWorkspaceState("test"), Dispatcher: &mcp.ExecDispatcher{Binary: bin}}
			stdio, err := fn(context.Background(), input, env)
			if err != nil {
				t.Fatal(err)
			}
			cliCalls := get()
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
			if len(cliCalls) != 1 || len(remoteCalls) != 1 {
				t.Fatalf("stdio=%+v %+v remote=%+v %+v", stdio, cliCalls, remote, remoteCalls)
			}
			a, b := cliCalls[0], remoteCalls[0]
			var ab, bb map[string]any
			_ = json.Unmarshal([]byte(a.body), &ab)
			_ = json.Unmarshal([]byte(b.body), &bb)
			if a.path != tc.path || b.path != tc.path || !reflect.DeepEqual(ab, bb) || ab["scope"] != "series" {
				t.Fatalf("stdio=%+v remote=%+v", a, b)
			}
			for _, res := range []*mcp.CallToolResult{stdio, remote} {
				raw, _ := json.Marshal(res.StructuredContent)
				if tc.code == "" {
					if res.IsError || !strings.Contains(string(raw), `"new_offset"`) {
						t.Fatalf("result=%s", raw)
					}
					continue
				}
				var v struct {
					Error struct {
						Code    string
						Details map[string]any
					}
				}
				_ = json.Unmarshal(raw, &v)
				if !res.IsError || v.Error.Code != "validation_error" || v.Error.Details["field"] != "scope" || v.Error.Details["code"] != tc.code {
					t.Fatalf("error=%s", raw)
				}
			}
		})
	}
}
