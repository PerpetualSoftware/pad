package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestResponseCapHelperProcess is the subprocess entry point used by the exec
// response-cap tests to emit exact stdout/stderr payloads and exit codes
// without relying on external shell utilities.
func TestResponseCapHelperProcess(t *testing.T) {
	if os.Getenv("PAD_TEST_RESPONSE_CAP_HELPER") != "1" {
		t.Skip("helper-process entry point")
	}
	mode := os.Getenv("PAD_TEST_RESPONSE_CAP_MODE")
	switch mode {
	case "stdout_4k_exit0":
		payload := fmt.Sprintf(`[{"ref":"TASK-1","content":%q}]`, strings.Repeat("a", 4096))
		_, _ = os.Stdout.WriteString(payload)
		os.Exit(0)
	case "stdout_4k_exit1":
		payload := fmt.Sprintf(`[{"ref":"TASK-1","content":%q}]`, strings.Repeat("a", 4096))
		_, _ = os.Stdout.WriteString(payload)
		_, _ = os.Stderr.WriteString("Error: failed after large output\n")
		os.Exit(1)
	case "stderr_2m_exit1":
		chunk := strings.Repeat("E", 4096)
		for written := 0; written < 2<<20; written += len(chunk) {
			_, _ = os.Stderr.WriteString(chunk)
		}
		os.Exit(1)
	case "stdout_small_exit0":
		_, _ = os.Stdout.WriteString(`{"ref":"TASK-1","title":"Small item"}`)
		os.Exit(0)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
}

func responseCapHelperBinary(t *testing.T, mode string) string {
	t.Helper()
	wrapper := filepath.Join(t.TempDir(), "pad")
	script := fmt.Sprintf(
		"#!/bin/sh\nPAD_TEST_RESPONSE_CAP_HELPER=1 PAD_TEST_RESPONSE_CAP_MODE=%q exec %q -test.run '^TestResponseCapHelperProcess$' -test.count=1\n",
		mode, os.Args[0],
	)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}
	return wrapper
}

func assertResponseTooLargeEnvelope(t *testing.T, res *CallToolResult, wantLimit int64) {
	t.Helper()
	if res == nil {
		t.Fatal("result is nil, want IsError too_large result")
	}
	if !res.IsError {
		t.Fatalf("IsError = false, want true (text=%q)", textOf(res))
	}
	env := decodeEnvelope(t, res)
	if env.Error.Code != ErrTooLarge {
		t.Fatalf("error.code = %q, want %q (message=%q)", env.Error.Code, ErrTooLarge, env.Error.Message)
	}
	var details struct {
		Reason     string `json:"reason"`
		LimitBytes int64  `json:"limit_bytes"`
	}
	if err := json.Unmarshal(env.Error.Details, &details); err != nil {
		t.Fatalf("decode error.details %q: %v", string(env.Error.Details), err)
	}
	if details.Reason != "response_too_large" {
		t.Errorf("details.reason = %q, want %q", details.Reason, "response_too_large")
	}
	if details.LimitBytes != wantLimit {
		t.Errorf("details.limit_bytes = %d, want %d", details.LimitBytes, wantLimit)
	}
}

func TestResponseCap_HTTPOverLimit(t *testing.T) {
	const limit int64 = 1024
	bigBody := fmt.Sprintf(`[{"ref":"TASK-1","title":"Big","content":%q}]`, strings.Repeat("x", 4096))
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(bigBody))
	})
	d := &HTTPHandlerDispatcher{
		Handler:          stub,
		UserResolver:     fixedUserResolver(&models.User{ID: "u1", Name: "Tester"}),
		ResponseMaxBytes: limit,
	}

	cases := []struct {
		name    string
		cmdPath []string
		input   map[string]any
	}{
		{
			name:    "routeTable collection list",
			cmdPath: []string{"collection", "list"},
			input:   map[string]any{"workspace": "ws"},
		},
		{
			name:    "item show without agent",
			cmdPath: []string{"item", "show"},
			input:   map[string]any{"workspace": "ws", "ref": "TASK-1"},
		},
		{
			name:    "item list summary branch",
			cmdPath: []string{"item", "list"},
			input:   map[string]any{"workspace": "ws"},
		},
		{
			name:    "item list with full=true",
			cmdPath: []string{"item", "list"},
			input:   map[string]any{"workspace": "ws", "full": true},
		},
		{
			name:    "item history",
			cmdPath: []string{"item", "history"},
			input:   map[string]any{"workspace": "ws", "ref": "TASK-1"},
		},
		{
			name:    "item update prefetch",
			cmdPath: []string{"item", "update"},
			input:   map[string]any{"workspace": "ws", "ref": "TASK-1", "title": "New"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithDispatchInput(context.Background(), tc.input)
			res, err := d.Dispatch(ctx, tc.cmdPath, nil)
			if err != nil {
				t.Fatalf("Dispatch returned err: %v", err)
			}
			assertResponseTooLargeEnvelope(t, res, limit)
		})
	}
}

func TestResponseCap_HTTPOverLimitErrorStatus(t *testing.T) {
	const limit int64 = 1024
	bigErrBody := fmt.Sprintf(`{"error":{"code":"internal_error","message":%q}}`, strings.Repeat("e", 4096))
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(bigErrBody))
	})
	d := &HTTPHandlerDispatcher{
		Handler:          stub,
		UserResolver:     fixedUserResolver(&models.User{ID: "u1", Name: "Tester"}),
		ResponseMaxBytes: limit,
	}

	ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": "ws", "ref": "TASK-1"})
	res, err := d.Dispatch(ctx, []string{"item", "show"}, nil)
	if err != nil {
		t.Fatalf("Dispatch returned err: %v", err)
	}
	assertResponseTooLargeEnvelope(t, res, limit)
}

// An oversized PATCH answer in bulk-update is that row's outcome: the write
// already ran, so the call must still report every row (earlier ones were
// written too), with a 2xx row counted as updated.
func TestResponseCap_BulkUpdateRowOutcome(t *testing.T) {
	const limit int64 = 1024
	big := strings.Repeat("x", 4096)
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/TASK-1"):
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"ref":"TASK-1","content":%q}`, big)
		case strings.HasSuffix(r.URL.Path, "/TASK-2"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"error":{"code":"internal_error","message":%q}}`, big)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ref":"TASK-3"}`))
		}
	})
	d := &HTTPHandlerDispatcher{
		Handler:          stub,
		UserResolver:     fixedUserResolver(&models.User{ID: "u1", Name: "Tester"}),
		ResponseMaxBytes: limit,
	}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "ws", "ref": []any{"TASK-1", "TASK-2", "TASK-3"}, "status": "done",
	})
	res, err := d.Dispatch(ctx, []string{"item", "bulk-update"}, nil)
	if err != nil {
		t.Fatalf("Dispatch returned err: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError = true, want a per-row report (text=%q)", textOf(res))
	}
	var got struct {
		Updated int `json:"updated"`
		Total   int `json:"total"`
		Results []struct {
			Ref     string `json:"ref"`
			Updated bool   `json:"updated"`
			Error   *struct {
				Code ErrorCode `json:"code"`
			} `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(textOf(res)), &got); err != nil {
		t.Fatalf("decode result %q: %v", textOf(res), err)
	}
	if got.Updated != 2 || got.Total != 3 || len(got.Results) != 3 {
		t.Fatalf("updated/total/rows = %d/%d/%d, want 2/3/3 (%s)", got.Updated, got.Total, len(got.Results), textOf(res))
	}
	if !got.Results[0].Updated || got.Results[0].Error != nil {
		t.Errorf("TASK-1 (2xx, oversized) = %+v, want updated", got.Results[0])
	}
	if got.Results[1].Updated || got.Results[1].Error == nil || got.Results[1].Error.Code != ErrTooLarge {
		t.Errorf("TASK-2 (5xx, oversized) = %+v, want error too_large", got.Results[1])
	}
	if !got.Results[2].Updated {
		t.Errorf("TASK-3 = %+v, want updated", got.Results[2])
	}
}

// A write the handler accepted has happened even when its answer is too big
// to return, so it is reported as done (result omitted), never as too_large,
// which would invite a retry that writes twice. A refused write is too_large.
func TestResponseCap_WriteOverflowReportsDone(t *testing.T) {
	const limit int64 = 1024
	big := strings.Repeat("x", 4096)
	for _, tc := range []struct {
		name   string
		status int
		wantOK bool
	}{
		{"accepted create", http.StatusCreated, true},
		{"refused create", http.StatusBadRequest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var method string
			stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method = r.Method
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"ref":"TASK-1","content":%q}`, big)
			})
			d := &HTTPHandlerDispatcher{
				Handler:          stub,
				UserResolver:     fixedUserResolver(&models.User{ID: "u1", Name: "Tester"}),
				ResponseMaxBytes: limit,
			}
			ctx := WithDispatchInput(context.Background(), map[string]any{
				"workspace": "ws", "collection": "tasks", "title": "Big",
			})
			res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
			if err != nil {
				t.Fatalf("Dispatch returned err: %v", err)
			}
			if method != http.MethodPost {
				t.Fatalf("handler saw %q, want POST", method)
			}
			if !tc.wantOK {
				assertResponseTooLargeEnvelope(t, res, limit)
				return
			}
			if res.IsError {
				t.Fatalf("IsError = true, want a done result with the body omitted (text=%q)", textOf(res))
			}
			var got struct {
				OK            bool   `json:"ok"`
				ResultOmitted bool   `json:"result_omitted"`
				Reason        string `json:"reason"`
				LimitBytes    int64  `json:"limit_bytes"`
			}
			raw, _ := json.Marshal(res.StructuredContent)
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode structured content %s: %v", raw, err)
			}
			if !got.OK || !got.ResultOmitted || got.Reason != "response_too_large" || got.LimitBytes != limit {
				t.Errorf("structured = %+v, want ok, result_omitted, response_too_large, %d", got, limit)
			}
		})
	}
}

func TestResponseCap_HTTPUnderLimit(t *testing.T) {
	const limit int64 = 1024
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/ws/collections", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"slug":"tasks","name":"Tasks"}]`))
	})
	mux.HandleFunc("/api/v1/workspaces/ws/items/TASK-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"i1","ref":"TASK-1","title":"Small","content":"Body","fields":"{\"status\":\"open\"}"}`))
	})
	mux.HandleFunc("/api/v1/workspaces/ws/items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"i1","ref":"TASK-1","title":"Small","content":"Body","fields":"{\"status\":\"open\"}"}]`))
	})
	mux.HandleFunc("/api/v1/workspaces/ws/items/TASK-1/versions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"v1","created_at":"2026-01-01T00:00:00Z","created_by":"u1","source":"api","change_summary":"init"}]`))
	})

	d := &HTTPHandlerDispatcher{
		Handler:          mux,
		UserResolver:     fixedUserResolver(&models.User{ID: "u1", Name: "Tester"}),
		ResponseMaxBytes: limit,
	}

	t.Run("routeTable collection list", func(t *testing.T) {
		ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": "ws"})
		res, err := d.Dispatch(ctx, []string{"collection", "list"}, nil)
		if err != nil || res.IsError {
			t.Fatalf("unexpected failure: err=%v res=%+v", err, res)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("StructuredContent = %T, want map[string]any", res.StructuredContent)
		}
		items, _ := sc["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items len = %d, want 1", len(items))
		}
		first, _ := items[0].(map[string]any)
		if first["slug"] != "tasks" {
			t.Errorf("slug = %v, want tasks", first["slug"])
		}
	})

	t.Run("item show without agent", func(t *testing.T) {
		ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": "ws", "ref": "TASK-1"})
		res, err := d.Dispatch(ctx, []string{"item", "show"}, nil)
		if err != nil || res.IsError {
			t.Fatalf("unexpected failure: err=%v res=%+v", err, res)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok || sc["ref"] != "TASK-1" {
			t.Fatalf("StructuredContent = %#v, want ref TASK-1", res.StructuredContent)
		}
	})

	t.Run("item list summary branch", func(t *testing.T) {
		ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": "ws"})
		res, err := d.Dispatch(ctx, []string{"item", "list"}, nil)
		if err != nil || res.IsError {
			t.Fatalf("unexpected failure: err=%v res=%+v", err, res)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("StructuredContent = %T, want map[string]any", res.StructuredContent)
		}
		items, _ := sc["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items len = %d, want 1", len(items))
		}
		first, _ := items[0].(map[string]any)
		if first["ref"] != "TASK-1" || first["content_preview"] != "Body" {
			t.Errorf("summary item = %#v, want ref=TASK-1 content_preview=Body", first)
		}
		if _, hasContent := first["content"]; hasContent {
			t.Errorf("summary item should not contain full content")
		}
	})

	t.Run("item list full=true", func(t *testing.T) {
		ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": "ws", "full": true})
		res, err := d.Dispatch(ctx, []string{"item", "list"}, nil)
		if err != nil || res.IsError {
			t.Fatalf("unexpected failure: err=%v res=%+v", err, res)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("StructuredContent = %T, want map[string]any", res.StructuredContent)
		}
		items, _ := sc["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items len = %d, want 1", len(items))
		}
		first, _ := items[0].(map[string]any)
		if first["ref"] != "TASK-1" || first["content"] != "Body" {
			t.Errorf("full item = %#v, want ref=TASK-1 content=Body", first)
		}
	})

	t.Run("item history", func(t *testing.T) {
		ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": "ws", "ref": "TASK-1"})
		res, err := d.Dispatch(ctx, []string{"item", "history"}, nil)
		if err != nil || res.IsError {
			t.Fatalf("unexpected failure: err=%v res=%+v", err, res)
		}
		sc, ok := res.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("StructuredContent = %T, want map[string]any", res.StructuredContent)
		}
		items, _ := sc["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items len = %d, want 1", len(items))
		}
		first, _ := items[0].(map[string]any)
		if first["id"] != "v1" {
			t.Errorf("version summary = %#v, want id=v1", first)
		}
	})
}

func TestResponseCap_ExecOverLimit(t *testing.T) {
	const limit int64 = 1024
	for _, tc := range []struct {
		name       string
		mode       string
		wantExitOK bool
	}{
		{"exit 0", "stdout_4k_exit0", true},
		{"exit 1", "stdout_4k_exit1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &ExecDispatcher{
				Binary:           responseCapHelperBinary(t, tc.mode),
				ResponseMaxBytes: limit,
			}
			res, err := d.Dispatch(context.Background(), []string{"item", "list"}, []string{"--format", "json"})
			if err != nil {
				t.Fatalf("Dispatch returned err: %v", err)
			}
			assertResponseTooLargeEnvelope(t, res, limit)
			// A child that finished may have been a write that was applied:
			// the envelope must say so, so the agent checks before retrying.
			env := decodeEnvelope(t, res)
			var details struct {
				ExitOK bool `json:"exit_ok"`
			}
			if err := json.Unmarshal(env.Error.Details, &details); err != nil {
				t.Fatalf("decode details: %v", err)
			}
			if details.ExitOK != tc.wantExitOK {
				t.Errorf("details.exit_ok = %v, want %v", details.ExitOK, tc.wantExitOK)
			}
			if tc.wantExitOK && !strings.Contains(env.Error.Hint, "has been applied") {
				t.Errorf("hint = %q, want it to say a write has been applied", env.Error.Hint)
			}
		})
	}
}

func TestResponseCap_ExecStderrBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d := &ExecDispatcher{
		Binary: responseCapHelperBinary(t, "stderr_2m_exit1"),
	}
	res, err := d.Dispatch(ctx, []string{"item", "list"}, []string{"--format", "json"})
	if err != nil {
		t.Fatalf("Dispatch returned err: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("context timed out (child may have blocked on stderr pipe): %v", ctx.Err())
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected IsError result, got %+v", res)
	}
	env := decodeEnvelope(t, res)
	totalLen := len(env.Error.Message) + len(env.Error.Hint)
	maxAllowed := int(stderrMaxBytes) + 4096
	if totalLen >= maxAllowed {
		t.Fatalf("error message+hint length = %d, want < %d", totalLen, maxAllowed)
	}
}

func TestResponseCap_ExecUnderLimit(t *testing.T) {
	d := &ExecDispatcher{
		Binary:           responseCapHelperBinary(t, "stdout_small_exit0"),
		ResponseMaxBytes: 1024,
	}
	res, err := d.Dispatch(context.Background(), []string{"item", "show"}, []string{"--format", "json", "--", "TASK-1"})
	if err != nil {
		t.Fatalf("Dispatch returned err: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("expected success, got %+v", res)
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok || sc["ref"] != "TASK-1" {
		t.Fatalf("StructuredContent = %#v, want ref TASK-1", res.StructuredContent)
	}
}

func TestResponseCap_HTTPResourceFetcherOverLimit(t *testing.T) {
	const limit int64 = 1024
	bigBody := fmt.Sprintf(`{"workspace":{"slug":"ws"},"pad":%q}`, strings.Repeat("b", 4096))
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bigBody))
	})
	d := &HTTPHandlerDispatcher{
		Handler:          stub,
		UserResolver:     fixedUserResolver(&models.User{ID: "u1"}),
		ResponseMaxBytes: limit,
	}
	f := NewHTTPResourceFetcher(d)
	_, err := f.Fetch(context.Background(), []string{"bootstrap", "--workspace", "ws", "--format", "json"})
	if err == nil || !strings.Contains(err.Error(), "exceeded") || !strings.Contains(err.Error(), strconv.FormatInt(limit, 10)) {
		t.Fatalf("Fetch err = %v, want error naming exceeded and %d-byte cap", err, limit)
	}
}

type guardAllowEntry struct {
	file   string
	fn     string
	reason string
}

func TestMCPResponseBuffersAreCapped(t *testing.T) {
	allowlist := []guardAllowEntry{
		{
			file:   "chatgpt_security.go",
			fn:     "chatGPTToolSchemes.ServeHTTP",
			reason: "wraps the outer /mcp handler and buffers only a tools/list response, bounded by the tool catalog",
		},
	}

	isAllowed := func(file, fn string) bool {
		for _, e := range allowlist {
			if e.file == file && e.fn == fn {
				return true
			}
		}
		return false
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/mcp: %v", err)
	}

	fset := token.NewFileSet()
	checkedFiles := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checkedFiles++
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		// Collect local identifiers bound to "net/http/httptest".
		httptestNames := map[string]bool{}
		for _, imp := range f.Imports {
			importPath, unqErr := strconv.Unquote(imp.Path.Value)
			if unqErr != nil || importPath != "net/http/httptest" {
				continue
			}
			if imp.Name != nil {
				httptestNames[imp.Name.Name] = true
			} else {
				httptestNames["httptest"] = true
			}
		}

		inspectNode := func(node ast.Node, enclosingFn string) {
			ast.Inspect(node, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if id, ok := sel.X.(*ast.Ident); ok && httptestNames[id.Name] && sel.Sel.Name == "NewRecorder" {
						if !isAllowed(name, enclosingFn) {
							pos := fset.Position(x.Pos())
							t.Errorf("%s:%d (%s): uncapped httptest.NewRecorder() call", name, pos.Line, enclosingFn)
						}
					}
					if name != "response_cap.go" && sel.Sel.Name == "ServeHTTP" {
						if !isAllowed(name, enclosingFn) {
							pos := fset.Position(x.Pos())
							t.Errorf("%s:%d (%s): direct ServeHTTP call outside response_cap.go", name, pos.Line, enclosingFn)
						}
					}
				case *ast.AssignStmt:
					if name == "response_cap.go" {
						return true
					}
					for _, lhs := range x.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok {
							continue
						}
						if sel.Sel.Name == "Stdout" || sel.Sel.Name == "Stderr" {
							if !isAllowed(name, enclosingFn) {
								pos := fset.Position(lhs.Pos())
								t.Errorf("%s:%d (%s): direct assignment to .%s outside response_cap.go", name, pos.Line, enclosingFn, sel.Sel.Name)
							}
						}
					}
				}
				return true
			})
		}

		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				inspectNode(d.Body, funcDeclName(d))
			default:
				inspectNode(d, "<package>")
			}
		}
	}

	if checkedFiles < 10 {
		t.Fatalf("parsed only %d non-test Go files; scan did not inspect package", checkedFiles)
	}
}

func funcDeclName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	return recvTypeName(d.Recv.List[0].Type) + "." + d.Name.Name
}

func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	default:
		return ""
	}
}
