package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
)

// TASK-2306. The MCP wire contract, as golden bytes: the `initialize` result
// and the full `tools/list` result, on BOTH transports, produced by the same
// bindings production uses (the real `pad mcp serve` subcommand for stdio;
// registerRemoteMCP behind NewRemoteTransport for remote /mcp).
//
// It exists so "this change is behavior-neutral" is a checked claim rather
// than an argument: an SDK bump, a type alias, a refactor of the result
// constructors must leave these files untouched. It is also the instrument
// that would have caught BUG-2302 (mcp-go injecting destructiveHint:true on
// every tool): a byte budget like prompt_budget_test cannot, because the
// payload changed meaning without changing much size.
//
// A DIFF HERE IS A WIRE-CONTRACT CHANGE. If it is intended (a catalog edit, an
// instructions.md edit), regenerate with
//
//	PAD_UPDATE_MCP_GOLDEN=1 go test ./cmd/pad/ -run TestMCPWireGolden
//
// review the testdata diff as the contract diff it is, and decide whether it
// owes a ToolSurfaceVersion bump. If it is NOT intended, that is the finding.
//
// The only field normalised is serverInfo.version, which is the build's
// version string and not part of the contract. The JSON is re-indented for
// readable diffs; indentation cannot change a token, so every key, value,
// escape and ordering is still compared exactly.

const mcpGoldenDir = "testdata/mcp_wire"

var goldenClientInfo = `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"golden","version":"1"}}`

func TestMCPWireGolden_Stdio(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "mcp", "serve")
	cmd.Env = append(os.Environ(), padHelperEnv+"=1", "HOME="+t.TempDir())
	cmd.Stdin = strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` + goldenClientInfo + "}\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pad mcp serve: %v\nstderr: %s", err, stderr.String())
	}
	byID := map[float64][]byte{}
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var msg struct {
			ID float64 `json:"id"`
		}
		line := append([]byte(nil), sc.Bytes()...)
		if json.Unmarshal(line, &msg) == nil && msg.ID != 0 {
			byID[msg.ID] = line
		}
	}
	compareGolden(t, "stdio_initialize.json", resultOf(t, byID[1]))
	compareGolden(t, "stdio_tools_list.json", resultOf(t, byID[2]))
}

func TestMCPWireGolden_Remote(t *testing.T) {
	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", Version: fullVersion(), Homepage: padHomepage, MaxDepth: -1})
	mcpSrv := mcpserver.NewServer(mcpserver.Options{Version: fullVersion()})
	// A zero dispatcher: initialize and tools/list never dispatch, and the
	// production one needs a live *server.Server that has no bearing on
	// either answer.
	if err := registerRemoteMCP(mcpSrv, doc, &mcpserver.HTTPHandlerDispatcher{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mcpserver.NewRemoteTransport(mcpSrv.MCP(), &padMCPGenerateOnlySessionIDManager{}))
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
		if resp.StatusCode/100 != 2 {
			t.Fatalf("POST %s: %d %s", body, resp.StatusCode, raw)
		}
		// A streamable response may arrive as one SSE event; take its data.
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

	compareGolden(t, "remote_initialize.json", resultOf(t, initRaw))
	compareGolden(t, "remote_tools_list.json", resultOf(t, listRaw))
}

// resultOf extracts a response's `result`, with serverInfo.version replaced
// by a placeholder. The rest of the bytes are passed through untouched.
func resultOf(t *testing.T, msg []byte) []byte {
	t.Helper()
	if len(msg) == 0 {
		t.Fatal("no response for this request")
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(msg, &env); err != nil || len(env.Result) == 0 {
		t.Fatalf("not a result response (err %v): %s", err, msg)
	}
	out := env.Result
	if v := fullVersion(); v != "" {
		out = bytes.Replace(out, []byte(`"version":`+mustJSON(t, v)), []byte(`"version":"<pad-version>"`), 1)
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, got, "", "  "); err != nil {
		t.Fatalf("%s: not JSON: %v", name, err)
	}
	pretty.WriteByte('\n')
	path := filepath.Join(mcpGoldenDir, name)
	if os.Getenv("PAD_UPDATE_MCP_GOLDEN") == "1" {
		if err := os.MkdirAll(mcpGoldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (generate it with PAD_UPDATE_MCP_GOLDEN=1)", path, err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Fatalf("%s: the MCP wire contract moved.\n%s\n"+
			"If this is intended, regenerate with PAD_UPDATE_MCP_GOLDEN=1, review the testdata diff as the "+
			"contract diff it is, and decide whether it owes a ToolSurfaceVersion bump.",
			path, firstDiff(want, pretty.Bytes()))
	}
}

// firstDiff names the first differing line, so a failure says where.
func firstDiff(want, got []byte) string {
	w, g := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + wl + "\n  got:  " + gl
		}
	}
	return "(no line differs; trailing bytes differ)"
}
