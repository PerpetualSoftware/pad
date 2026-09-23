package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2829: an HTTP 413 is a deliberate server cap, and both transports used
// to report it as server_error, which reads as transient and invites a retry
// that fails identically.

func reasonOf(t *testing.T, env ErrorEnvelope) string {
	t.Helper()
	if len(env.Error.Details) == 0 {
		return ""
	}
	var d struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(env.Error.Details, &d); err != nil {
		t.Fatalf("details is not an object with a reason: %s", env.Error.Details)
	}
	return d.Reason
}

// TestA413IsTooLargeOnTheRemoteTransport covers each producer reachable from
// the catalog (the enumeration is on BUG-2829's trail), by its real body shape.
func TestA413IsTooLargeOnTheRemoteTransport(t *testing.T) {
	for _, tc := range []struct {
		code, message string
	}{
		{"rename_cascade_too_large", "Renaming this item would rewrite links in too many items."},
		{"event_payload_too_large", "This change would record an item.updated event larger than the server will store in one row."},
		{"too_large", "Artifact body exceeds the size limit"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": tc.code, "message": tc.message}})
			env := decodeEnvelope(t, classifyHTTPStatus(context.Background(), "item update", http.StatusRequestEntityTooLarge, body, nil))
			if env.Error.Code != ErrTooLarge {
				t.Fatalf("code = %q, want %q", env.Error.Code, ErrTooLarge)
			}
			if got := reasonOf(t, env); got != tc.code {
				t.Errorf("details.reason = %q, want the server's own code %q", got, tc.code)
			}
			if env.Error.Message != tc.message {
				t.Errorf("message = %q, want the server's message %q (it names what was measured)", env.Error.Message, tc.message)
			}
			if !strings.Contains(env.Error.Hint, "not a fault") {
				t.Errorf("hint = %q, want the too_large guidance", env.Error.Hint)
			}
		})
	}

	// An unstructured 413 body (a proxy in front of Pad, say) still gets the
	// code; it just has no reason to report.
	env := decodeEnvelope(t, classifyHTTPStatus(context.Background(), "item update", http.StatusRequestEntityTooLarge, []byte("Request Entity Too Large"), nil))
	if env.Error.Code != ErrTooLarge || reasonOf(t, env) != "" {
		t.Fatalf("unstructured 413: code %q reason %q, want too_large and no reason", env.Error.Code, reasonOf(t, env))
	}
}

// TestAnOversizedImportIsTooLargeThroughTheDispatcher is the binding test
// (CONVE-19): a real server with a small import cap, driven through the real
// HTTP dispatcher's `item import` special case, which reaches the classifier
// by its own route. The classifier rows above vouch for the mapping, not for
// every door that calls it.
func TestAnOversizedImportIsTooLargeThroughTheDispatcher(t *testing.T) {
	srv, st := newPadServer(t)
	srv.SetImportArtifactMaxBytes(256)

	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces", map[string]any{"name": "DocApp", "template": "startup"})
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(wsRec.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode workspace: %v", err)
	}
	user, err := st.CreateUser(models.UserCreate{Email: "dave@example.com", Name: "Dave", Password: "irrelevant"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: fixedUserResolver(user)}

	data, err := artifact.Encode(artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Oversized Convention",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all", "priority": "must"},
		Body:          strings.Repeat("An oversized body. ", 100),
	})
	if err != nil {
		t.Fatalf("encode artifact: %v", err)
	}
	if len(data) <= 256 {
		t.Fatalf("artifact is %d bytes, not over the 256-byte cap; this test could not have discriminated", len(data))
	}
	ctx := WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug, "artifact": string(data)})
	res, err := d.Dispatch(ctx, []string{"item", "import"}, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	env := decodeEnvelope(t, res)
	if env.Error.Code != ErrTooLarge {
		t.Fatalf("code = %q, want %q: the dispatcher reported a deliberate cap as %q", env.Error.Code, ErrTooLarge, env.Error.Code)
	}
	if got := reasonOf(t, env); got != "too_large" {
		t.Errorf("details.reason = %q, want the server's code too_large", got)
	}
}

// TestA413IsTooLargeOnTheStdioTransport drives the marker the CLI root writes
// through the stdio classifier, with the same guidance the remote door gives.
func TestA413IsTooLargeOnTheStdioTransport(t *testing.T) {
	var stderr bytes.Buffer
	stderr.WriteString("Error: Renaming this item would rewrite links in too many items.\n")
	cli.WriteTooLargeError(&stderr, &cli.APIError{
		Code: "rename_cascade_too_large", Message: "Renaming this item would rewrite links in too many items.", Status: http.StatusRequestEntityTooLarge,
	})
	res := extractStructuredCLIError(stderr.String())
	if res == nil {
		t.Fatal("the stdio classifier did not lift the too_large marker; it would fall back to server_error")
	}
	env := decodeEnvelope(t, res)
	if env.Error.Code != ErrTooLarge {
		t.Fatalf("code = %q, want %q", env.Error.Code, ErrTooLarge)
	}
	if got := reasonOf(t, env); got != "rename_cascade_too_large" {
		t.Errorf("details.reason = %q, want rename_cascade_too_large", got)
	}
	if env.Error.Hint != tooLargeHint {
		t.Errorf("stdio hint = %q, want the same guidance the remote door gives", env.Error.Hint)
	}
}

func TestTooLargeHintIsTheSameOnBothTransports(t *testing.T) {
	if cli.TooLargeHint != tooLargeHint {
		t.Fatalf("cli.TooLargeHint and mcp.tooLargeHint differ:\n cli: %q\n mcp: %q", cli.TooLargeHint, tooLargeHint)
	}
}

// TestEveryCLIMarkerCodeIsInTheStdioVocabulary is the lead's ask on BUG-2829:
// a code the CLI can write into a structured marker must be one the stdio
// classifier lifts, or that transport silently falls back to server_error
// while the remote one reports the real code — the exact split BUG-3147 and
// this bug each found once by accident.
//
// Two halves. The table calls every marker writer and checks the code it
// actually wrote. The completeness check parses internal/cli and finds every
// function that references StructuredErrorMarker, so a NEW writer that is not
// added to the table fails here rather than escaping the first half.
func TestEveryCLIMarkerCodeIsInTheStdioVocabulary(t *testing.T) {
	api := func(code string) *cli.APIError {
		return &cli.APIError{Code: code, Message: "m", Status: http.StatusBadRequest}
	}
	writers := map[string]func(*bytes.Buffer){
		"WriteOpenChildrenError": func(w *bytes.Buffer) {
			cli.WriteOpenChildrenError(w, api("open_children"), &cli.OpenChildrenDetails{})
		},
		"WriteUpdateConflictError": func(w *bytes.Buffer) {
			cli.WriteUpdateConflictError(w, api("update_conflict"), &cli.UpdateConflictDetails{})
		},
		"WriteContentPendingFlushError": func(w *bytes.Buffer) {
			cli.WriteContentPendingFlushError(w, api("content_pending_flush"))
		},
		"WriteStoredStateUnreadableError": func(w *bytes.Buffer) {
			cli.WriteStoredStateUnreadableError(w, errors.New("unreadable"))
		},
		"WritePlanLimitError": func(w *bytes.Buffer) {
			cli.WritePlanLimitError(w, api("plan_limit_exceeded"))
		},
		"WriteRateLimitedError": func(w *bytes.Buffer) {
			cli.WriteRateLimitedError(w, api("rate_limited"))
		},
		"WriteTooLargeError": func(w *bytes.Buffer) {
			cli.WriteTooLargeError(w, api("rename_cascade_too_large"))
		},
	}

	for name, write := range writers {
		var buf bytes.Buffer
		write(&buf)
		var payload string
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.HasPrefix(line, cli.StructuredErrorMarker) {
				payload = strings.TrimPrefix(line, cli.StructuredErrorMarker)
			}
		}
		if payload == "" {
			t.Errorf("%s wrote no structured marker line", name)
			continue
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &env); err != nil || env.Error.Code == "" {
			t.Errorf("%s wrote an unreadable marker %q: %v", name, payload, err)
			continue
		}
		if _, ok := allowedStructuredErrorCodes[env.Error.Code]; !ok {
			t.Errorf("%s writes code %q, which allowedStructuredErrorCodes does not contain: stdio would report server_error for it", name, env.Error.Code)
		}
	}

	found := markerWritersInCLI(t)
	var missing []string
	for _, name := range found {
		if _, ok := writers[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("internal/cli functions reference StructuredErrorMarker but are not in this test's table: %v", missing)
	}
	if len(found) < len(writers) {
		t.Errorf("the source scan found %d marker writers %v but the table has %d; the scan is not seeing what it should", len(found), found, len(writers))
	}
}

// markerWritersInCLI returns every non-test top-level function in internal/cli
// whose body references the StructuredErrorMarker identifier.
func markerWritersInCLI(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "cli")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			uses := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "StructuredErrorMarker" {
					uses = true
				}
				return !uses
			})
			if uses {
				names = append(names, fn.Name.Name)
			}
		}
	}
	sort.Strings(names)
	return names
}
