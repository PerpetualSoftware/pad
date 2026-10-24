package server

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// TASK-4041: chat has no kernel event, activity, outbox or webhook effects.
func TestChatEmitsNoKernelEvent(t *testing.T) {
	f := newChatFixture(t)
	bus := events.New()
	f.srv.SetEventBus(bus)
	ch, _, _ := bus.Subscribe(context.Background(), f.wsID)
	defer bus.Unsubscribe(ch)
	mark := f.mark()
	counts := func() []int {
		out := []int{}
		for _, table := range []string{"activities", "event_outbox", "webhook_deliveries"} {
			var n int
			if err := f.srv.store.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}
	before := counts()
	p := f.ensure("rook")

	for _, tc := range []struct {
		body   map[string]any
		status int
	}{
		{map[string]any{"body": "first", "op_key": "once"}, 201},
		{map[string]any{"body": "replay", "op_key": "once"}, 200},
		{map[string]any{"body": "reply", "reply_to_seq": 1}, 201},
	} {
		f.must(f.request("POST", p+"/messages", "rook", tc.body), tc.status, "send")
	}

	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "human notice"}), 201, "human send")
	f.must(f.request("DELETE", p+"/messages/1", "", nil), 200, "delete")
	f.must(f.request("POST", p+"/read", "", map[string]any{"seq": 2}), 200, "read")
	for _, path := range []string{chatBase, p, p + "/messages", "/api/v1/me/chat/unread"} {
		f.must(f.request("GET", path, "", nil), 200, "get")
	}
	if after := counts(); !reflect.DeepEqual(before, after) {
		t.Fatalf("chat effects: before %v after %v", before, after)
	}
	for _, n := range f.srv.watchEvents.EventsSince(mark) {
		if n.Kind != watchevents.KindChat {
			t.Fatalf("non-chat watch event: %+v", n)
		}
	}
	select {
	case ev := <-ch:
		t.Fatalf("chat emitted event: %+v", ev)
	default:
	}
}
func TestChat_GuardHandlersDoNotReferenceEmitters(t *testing.T) {
	files, err := filepath.Glob("handlers_chat*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || (path != "handlers_chat.go" && !strings.HasPrefix(path, "handlers_chat_")) {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			name := id.Name
			if name == "events" || name == "watchEvents" || name == "goAsync" || strings.HasPrefix(name, "publish") || strings.HasPrefix(name, "logActivity") || strings.HasPrefix(name, "outbox") || strings.Contains(name, "Webhook") {
				t.Errorf("%s references emitter %s", path, name)
			}
			return true
		})
	}
}
func TestChat_ResponsesNeverCarryRawIdentifiers(t *testing.T) {
	f := newChatFixture(t)
	p := f.ensure("rook")
	f.must(f.request("POST", p+"/messages", "", map[string]any{"body": "[[TASK-999999]]", "context_workspace": f.wsSlug}), 201, "seed context")
	for _, tc := range chatRouteCases() {
		rr := rawReq(f.srv, tc.method, tc.path, f.ownerTok, "", tc.body)
		if rr.Code >= 300 {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, val := range x {
					for _, bad := range []string{"owner_user_id", "dm_key", "author_user_id", "context_workspace_id", "op_key"} {
						if k == bad {
							t.Errorf("%s %s leaks %s", tc.method, tc.path, k)
						}
					}
					walk(val)
				}
			case []any:
				for _, val := range x {
					walk(val)
				}
			}
		}
		out := chatObject(t, rr)
		walk(out)
		if tc.method == "GET" && strings.HasSuffix(tc.path, "/messages") {
			var bad []string
			nullLists(reflect.TypeOf(chatMessagesResponse{}), out, "page", &bad)
			if len(bad) > 0 {
				t.Fatal(fmt.Sprint(bad))
			}
		}
	}
}

func checkChatRequestFields(t *testing.T, re *regexp.Regexp) {
	file, err := parser.ParseFile(token.NewFileSet(), "handlers_chat.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok {
			return true
		}
		for _, f := range st.Fields.List {
			for _, name := range f.Names {
				if re.MatchString(name.Name) {
					t.Error(name.Name)
				}
			}
			if f.Tag != nil && re.MatchString(f.Tag.Value) {
				t.Error(f.Tag.Value)
			}
		}
		return true
	})
}

func checkChatReplayPublicationGuard(t *testing.T) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "handlers_chat.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	// TASK-4041, CONVE-12: both the !Replayed guard and the store's empty
	// replay recipients suppress publication. They produce the same end state,
	// so no bus assertion can distinguish them; the AST pins the guard itself.
	var handler *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "handleSendChatMessage" {
			handler = fn
		}
	}
	if handler == nil || handler.Body == nil {
		t.Fatal("missing handleSendChatMessage")
	}
	var sendEnd, errorReturnEnd token.Pos
	guarded := false
	for _, stmt := range handler.Body.List {
		if assign, ok := stmt.(*ast.AssignStmt); ok {
			for _, rhs := range assign.Rhs {
				if call, ok := rhs.(*ast.CallExpr); ok {
					if target, ok := call.Fun.(*ast.SelectorExpr); ok && target.Sel.Name == "SendConversationMessage" {
						sendEnd = assign.End()
					}
				}
			}
		}
		branch, ok := stmt.(*ast.IfStmt)
		if !ok || sendEnd == token.NoPos || branch.Pos() <= sendEnd {
			continue
		}
		if cond, ok := branch.Cond.(*ast.BinaryExpr); ok && cond.Op == token.NEQ {
			left, lok := cond.X.(*ast.Ident)
			right, rok := cond.Y.(*ast.Ident)
			if lok && rok && left.Name == "err" && right.Name == "nil" && len(branch.Body.List) > 0 {
				if _, ok := branch.Body.List[len(branch.Body.List)-1].(*ast.ReturnStmt); ok {
					errorReturnEnd = branch.End()
				}
			}
		}
		negation, ok := branch.Cond.(*ast.UnaryExpr)
		if !ok || negation.Op != token.NOT || errorReturnEnd == token.NoPos || branch.Pos() <= errorReturnEnd {
			continue
		}
		field, ok := negation.X.(*ast.SelectorExpr)
		if !ok || field.Sel.Name != "Replayed" {
			continue
		}
		result, ok := field.X.(*ast.Ident)
		if !ok || result.Name != "result" {
			continue
		}
		ast.Inspect(branch.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if target, ok := call.Fun.(*ast.SelectorExpr); ok && target.Sel.Name == "notifyChatMessage" {
					guarded = true
				}
			}
			return true
		})
	}
	if !guarded {
		t.Fatal("handleSendChatMessage must guard publication with !result.Replayed after the SendConversationMessage error return")
	}
}
