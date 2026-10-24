package main

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
	"github.com/PerpetualSoftware/pad/internal/models"
)

func chatSessionEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	socket := filepath.Join(t.TempDir(), "sock")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)
	if _, err := cli.WriteArmState(); err != nil {
		t.Fatal(err)
	}
}

func TestChatDirectionRootCommandBinding(t *testing.T) {
	chatSessionEnv(t)
	oldFormat, oldWorkspace, oldURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = oldFormat, oldWorkspace, oldURL })
	for _, tc := range []struct {
		name string
		args []string
		on   bool
	}{
		{"arm", []string{"chat", "arm", "--accept-instructions"}, true},
		{"disarm", []string{"chat", "disarm"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newRootCmd()
			var errout bytes.Buffer
			root.SetErr(&errout)
			root.SetArgs(tc.args)
			captureStdout(t, func() {
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
			})
			if cli.ChatDirectionOn() != tc.on {
				t.Fatalf("chat direction = %v, want %v", cli.ChatDirectionOn(), tc.on)
			}
			if cli.SessionArmState() != cli.LocalArmOn {
				t.Fatal("chat command changed delivery consent")
			}
		})
	}
}

func TestChatArmRequiresFlag(t *testing.T) {
	chatSessionEnv(t)
	cmd := newChatArmCmd()
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "pad session arm") {
		t.Fatal(err)
	}
	if cli.ChatDirectionOn() {
		t.Fatal("bare arm wrote")
	}
}
func TestChatArmPrintsWarningEveryTime(t *testing.T) {
	chatSessionEnv(t)
	old := formatFlag
	t.Cleanup(func() { formatFlag = old })
	for _, format := range []string{"text", "text", "json"} {
		formatFlag = format
		cmd := newChatArmCmd()
		var errout bytes.Buffer
		cmd.SetErr(&errout)
		cmd.SetArgs([]string{"--accept-instructions"})
		out := captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
		})
		for _, phrase := range []string{"instructions sent through Pad chat will drive this agent", "Pad does not authenticate the sender", "any process holding your Pad credential can send as you", "this session only", "pad session arm", "confirmed in-session"} {
			if !strings.Contains(errout.String(), phrase) {
				t.Fatalf("missing %q: %s", phrase, errout.String())
			}
		}
		if format == "json" {
			var v map[string]any
			if err := json.Unmarshal([]byte(out), &v); err != nil {
				t.Fatal(err)
			}
			if v["warning"] != strings.TrimSpace(errout.String()) {
				t.Fatal(out)
			}
		}
		if !cli.ChatDirectionOn() {
			t.Fatal("readback off")
		}
	}
}
func TestChatArmHeadlessRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	t.Setenv("CLAUDE_PID", "1")
	cmd := newChatArmCmd()
	cmd.SetArgs([]string{"--accept-instructions"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("headless allowed")
	}
	if cli.ChatDirectionOn() {
		t.Fatal("headless on")
	}
}
func TestChatDisarmLeavesPushArm(t *testing.T) {
	chatSessionEnv(t)
	cli.WriteChatDirection()
	if err := newChatDisarmCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if cli.ChatDirectionOn() || cli.SessionArmState() != cli.LocalArmOn {
		t.Fatal("wrong flags")
	}
}
func TestSessionDisarmClearsDirection(t *testing.T) {
	chatSessionEnv(t)
	path, err := cli.WriteChatDirection()
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionDisarmCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("direction file survived: %v", err)
	}
}
func TestSessionResetClearsDirection(t *testing.T) {
	chatSessionEnv(t)
	path, err := cli.WriteChatDirection()
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionResetCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("direction file survived: %v", err)
	}
}

// DOC-4036 pins dispatch destinations, not cmdhelp visibility: MCP cannot
// enable direction even when chat arm/disarm are ordinary visible CLI commands.
// There is no generic raw-command route: Register registers only the catalog
// and pad_set_workspace (state + a fixed bootstrap fetch). makeFanOutHandler
// rejects undeclared input keys and bounds "action" to that tool's Actions
// table; chatGPTHandler fixes its source action. passThrough/refAsItemRef
// close over catalog-owned cmdPaths.
// The input-dependent candidates are item link/unlink (link_type selects the
// fixed itemLinkRoutes table; every advertised type is exercised below), and
// pad_playbook.run's args/raw_args (playbook argument data; BOTH dispatcher
// branches use the literal [playbook run], never a caller-supplied cmdPath).
// ExecDispatcher appends CLI args to the catalog path without a shell;
// HTTPHandlerDispatcher resolves that path in routeTable. Resource URIs select
// fixed read commands, ExecBootstrapFetcher uses [bootstrap], and the exec
// workspace-lister side channel uses [workspace list]. Prompts/meta do not run
// caller commands. No args, command, raw_args, or request cmdPath passthrough
// can select an uncatalogued CLI path.
func TestChatArmHasNoMCPPath(t *testing.T) {
	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
	rec := &recordingDispatcher{}

	// Test-only dispatch accessor: wrap the SAME Actions maps captured by
	// Register/RegisterChatGPTCatalog, keeping their real action functions and
	// fan-out handlers. Registered schemas do not expose cmdPaths. Replace only
	// the terminal dispatcher so enumeration cannot execute production writes.
	// Restore whole maps; never mutate the original catalog's maps in place.
	original := mcp.Catalog
	mcp.Catalog = append([]mcp.ToolDef(nil), original...)
	t.Cleanup(func() { mcp.Catalog = original })
	for i := range mcp.Catalog {
		actions := maps.Clone(mcp.Catalog[i].Actions)
		for name, fn := range actions {
			toolName, actionName := mcp.Catalog[i].Name, name
			actions[name] = func(ctx context.Context, input map[string]any, env mcp.ActionEnv) (*mcp.CallToolResult, error) {
				originalEnv := env
				env.Dispatcher = rec
				res, err := fn(ctx, input, env)
				// playbook.run alone branches on the concrete dispatcher type.
				// Exercise its HTTP branch too, against the in-memory sink below.
				if _, isHTTP := originalEnv.Dispatcher.(*mcp.HTTPHandlerDispatcher); isHTTP && toolName == "pad_playbook" && actionName == "run" && err == nil && res != nil && !res.IsError {
					return fn(ctx, input, originalEnv)
				}
				return res, err
			}
		}
		mcp.Catalog[i].Actions = actions
	}

	for _, transport := range []string{"stdio", "remote-http", "chatgpt"} {
		t.Run(transport, func(t *testing.T) {
			srv := mcp.NewServer(mcp.Options{})
			playbookRunRequests := 0
			var err error
			switch transport {
			case "stdio":
				// The tool registration entry point used by pad mcp serve.
				_, err = mcp.Register(srv.MCP(), mcp.RegistryOptions{
					Doc: doc, Workspace: mcp.NewWorkspaceState("test"), Dispatcher: rec,
				})
			case "remote-http":
				err = registerRemoteMCP(srv, doc, &mcp.HTTPHandlerDispatcher{
					UserResolver: func(context.Context) *models.User { return &models.User{ID: "test-user"} },
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						playbookRunRequests++
						if r.Method != http.MethodPost || r.URL.Path != "/api/v1/workspaces/test/playbooks/TASK-1/run" {
							t.Errorf("playbook args selected unexpected route: %s %s", r.Method, r.URL.Path)
						}
						var body struct {
							Args    map[string]any `json:"args"`
							RawArgs []string       `json:"raw_args"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if body.Args["command"] != "chat arm" || strings.Join(body.RawArgs, " ") != "chat arm --accept-instructions" {
							t.Errorf("playbook command-like arguments must remain data: %+v", body)
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"ok":true}`))
					}),
				})
			case "chatgpt":
				srv, err = newChatGPTMCPServer(doc, rec)
			}
			if err != nil {
				t.Fatal(err)
			}
			tools := srv.MCP().ListTools()
			if len(tools) == 0 {
				t.Fatal("must enumerate real registered tools")
			}
			for name, tool := range tools {
				if name == "pad_set_workspace" {
					continue // Not a catalog action; bounded state/bootstrap helper above.
				}
				var schema struct {
					Properties map[string]struct{ Enum []string }
				}
				raw, err := json.Marshal(tool.Tool.InputSchema)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &schema); err != nil {
					t.Fatal(err)
				}
				actions := schema.Properties["action"].Enum
				if transport == "chatgpt" {
					// Each projection tool fixes exactly one source action.
					for _, entry := range mcp.ChatGPTCatalog {
						if entry.Name == name {
							actions = []string{entry.Source.Action}
						}
					}
				}
				if len(actions) == 0 {
					t.Fatalf("%s has no enumerated actions", name)
				}
				for _, action := range actions {
					types := []string{""}
					if action == "link" || action == "unlink" {
						types = schema.Properties["link_type"].Enum
						if len(types) == 0 {
							t.Fatal("missing link_type enum")
						}
					}
					for _, linkType := range types {
						t.Run(name+"/"+action+"/"+linkType, func(t *testing.T) {
							input := map[string]any{}
							for key, value := range chatMCPDispatchFixture() {
								if _, declared := schema.Properties[key]; declared {
									input[key] = value
								}
							}
							if transport != "chatgpt" {
								input["action"] = action
							}
							if linkType != "" {
								input["link_type"] = linkType
							}
							rec.take()
							req := mcpgo.CallToolRequest{}
							req.Params.Name, req.Params.Arguments = name, input
							req.Params.RawArguments, _ = json.Marshal(input)
							res, err := tool.Handler(context.Background(), req)
							if err != nil {
								t.Fatal(err)
							}
							if res == nil || res.IsError {
								t.Fatalf("action did not resolve: %+v", res)
							}
							calls := rec.take()
							inline := name == "pad_meta" && (action == "server-info" || action == "version" || action == "tool-surface")
							if inline && len(calls) == 0 {
								t.Log("inline (no cmdPath)")
							}
							if len(calls) == 0 && !inline {
								t.Fatal("action never reached cmdPath dispatch")
							}
							for _, call := range calls {
								path := strings.Join(call.Path, " ")
								t.Logf("cmdPath %s", path)
								if path == "chat arm" || path == "chat disarm" {
									t.Errorf("MCP %s.%s dispatches forbidden cmdPath %q", name, action, path)
								}
							}
						})
					}
				}
			}
			if transport == "remote-http" && playbookRunRequests != 1 {
				t.Errorf("playbook HTTP branch reached its fixed route %d times, want 1", playbookRunRequests)
			}
		})
	}
}

func chatMCPDispatchFixture() map[string]any {
	return map[string]any{
		"workspace": "test", "target": "C12", "body": "hello", "seq": float64(4), "to": float64(4),
		"email": "test@example.com", "name": "test", "slug": "test", "query": "test",
		"ref": "TASK-1", "refs": []string{"TASK-1", "TASK-2"}, "collection": "tasks",
		"title": "test", "target_collection": "ideas", "message": "test", "summary": "test",
		"decision": "test", "code": "123456", "attachment_id": "att-1", "reminder_id": "rem-1",
		"remind_at": "2026-08-01T09:00:00Z", "text": "test", "comment_id": "c-1",
		"artifact": "test artifact", "pr": "https://github.com/example/repo/pull/1",
		// The apparent raw-command candidate must remain playbook data.
		"raw_args": []string{"chat", "arm", "--accept-instructions"},
		"args":     map[string]any{"command": "chat arm"},
	}
}
