package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
)

// TASK-3321 U1: the ChatGPT catalog's wire contract, pinned like /mcp's
// (TestMCPWireGolden_*). /mcp's own golden files are untouched by this unit,
// which is the "byte-identical" half of the ruling; these are the new pair.
func TestChatGPTWireGolden(t *testing.T) {
	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", Version: fullVersion(), Homepage: padHomepage, MaxDepth: -1})
	srv, err := newChatGPTMCPServer(doc, &mcpserver.HTTPHandlerDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mcpserver.NewRemoteTransport(srv.MCP(), &padMCPGenerateOnlySessionIDManager{}))
	defer ts.Close()
	post := func(body, session string) ([]byte, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			for _, l := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(l, "data: ") {
					raw = []byte(strings.TrimPrefix(l, "data: "))
				}
			}
		}
		return raw, resp.Header.Get("Mcp-Session-Id")
	}
	initRaw, session := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+goldenClientInfo+`}`, "")
	post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, session)
	listRaw, _ := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, session)
	compareGolden(t, "chatgpt_initialize.json", resultOf(t, initRaw))
	compareGolden(t, "chatgpt_tools_list.json", resultOf(t, listRaw))
}

// recordingDispatcher records every dispatch: the CLI path, its args, and
// the dispatch input the HTTP dispatcher would read from the context.
type recordingDispatcher struct {
	mu    sync.Mutex
	calls []recordedDispatch
}

type recordedDispatch struct {
	Path  []string
	Args  []string
	Input map[string]any
}

func (d *recordingDispatcher) Dispatch(ctx context.Context, cmdPath, args []string) (*mcp.CallToolResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, recordedDispatch{
		Path:  append([]string(nil), cmdPath...),
		Args:  append([]string(nil), args...),
		Input: mcpserver.DispatchInputFromContext(ctx),
	})
	return mcp.NewToolResultText(`{"ok":true}`), nil
}

func (d *recordingDispatcher) take() []recordedDispatch {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.calls
	d.calls = nil
	return out
}

// TestChatGPTCatalog_DispatchesExactlyAsItsSource proves the projection
// claim: calling a ChatGPT tool produces the SAME dispatch (CLI path, args
// and dispatch input) as calling its /mcp source with the same arguments
// plus its action and Fixed inputs. Both servers share one cmdhelp doc and
// one dispatcher, so any difference is the projection's.
func TestChatGPTCatalog_DispatchesExactlyAsItsSource(t *testing.T) {
	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", Version: fullVersion(), Homepage: padHomepage, MaxDepth: -1})
	rec := &recordingDispatcher{}
	gpt, err := newChatGPTMCPServer(doc, rec)
	if err != nil {
		t.Fatal(err)
	}
	mcpSrv := mcpserver.NewServer(mcpserver.Options{Version: fullVersion()})
	if _, err := mcpserver.Register(mcpSrv.MCP(), mcpserver.RegistryOptions{
		Doc: doc, Workspace: mcpserver.NewSharedWorkspaceState(), Dispatcher: rec, PadVersion: fullVersion(),
	}); err != nil {
		t.Fatal(err)
	}
	sample := map[string]any{
		"workspace": "ws", "ref": "TASK-1", "query": "login", "collection": "tasks",
		"title": "A title", "message": "A comment", "target": "TASK-2", "content": "Body",
		"status": "open", "limit": float64(5), "link_type": "blocks",
	}
	call := func(s *server.MCPServer, tool string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		st := s.GetTool(tool)
		if st == nil {
			t.Fatalf("tool %s not registered", tool)
		}
		raw, _ := json.Marshal(args)
		req := mcp.CallToolRequest{}
		req.Params.Name = tool
		req.Params.Arguments = args
		req.Params.RawArguments = raw
		res, err := st.Handler(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return res
	}
	for _, e := range mcpserver.ChatGPTCatalog {
		t.Run(e.Name, func(t *testing.T) {
			args := map[string]any{}
			for _, p := range e.Required {
				v, ok := sample[p]
				if !ok {
					t.Fatalf("no sample value for required %q", p)
				}
				args[p] = v
			}
			srcArgs := map[string]any{}
			for k, v := range args {
				srcArgs[k] = v
			}
			for k, v := range e.Fixed {
				srcArgs[k] = v
			}
			srcTool := e.Source.Tool
			if e.Source.Action != "" {
				srcArgs["action"] = e.Source.Action
			}
			rec.take()
			gotRes := call(gpt.MCP(), e.Name, args)
			got := rec.take()
			wantRes := call(mcpSrv.MCP(), srcTool, srcArgs)
			want := rec.take()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("dispatch differs from %s:\n  chatgpt: %+v\n  source:  %+v", e.Source, got, want)
			}
			if len(want) == 0 && !reflect.DeepEqual(gotRes, wantRes) {
				t.Errorf("non-dispatching tool result differs from %s:\n  chatgpt: %+v\n  source:  %+v", e.Source, gotRes, wantRes)
			}
			if gotRes.IsError {
				t.Errorf("%s with its required args returned an error: %+v", e.Name, gotRes.Content)
			}
		})
	}
}

// A ChatGPT tool refuses a parameter it does not declare, even one its /mcp
// source accepts: the narrowed schema is the contract.
func TestChatGPTCatalog_RefusesUndeclaredParams(t *testing.T) {
	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", Version: fullVersion(), Homepage: padHomepage, MaxDepth: -1})
	rec := &recordingDispatcher{}
	gpt, err := newChatGPTMCPServer(doc, rec)
	if err != nil {
		t.Fatal(err)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "update_item"
	req.Params.Arguments = map[string]any{"workspace": "ws", "ref": "TASK-1", "overwrite_pending_edits": true}
	res, err := gpt.MCP().GetTool("update_item").Handler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("update_item accepted overwrite_pending_edits, which it does not declare")
	}
	if n := len(rec.take()); n != 0 {
		t.Fatalf("a refused call still dispatched %d time(s)", n)
	}
}
