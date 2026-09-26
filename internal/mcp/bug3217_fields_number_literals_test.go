package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// BUG-3217. mcp-go decodes a tool call's arguments with a plain
// json.Unmarshal, so a `fields` number reached the dispatcher as a float64:
// 9007199254740993 was stored as 9007199254740992, and 1.10 as 1.1. These
// tests send RAW JSON-RPC through a real mcp-go server, because a test that
// builds the arguments map itself hands the handler a value mcp-go never
// would and cannot see the decode at all.

func toolsCall(id int, args string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"pad_item","arguments":%s}}`, id, args))
}

func respText(t *testing.T, resp any) string {
	t.Helper()
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMCPFieldsKeepNumberLiterals_Remote drives the remote door: catalog →
// HTTPHandlerDispatcher → the real handler chain → the store, and reads the
// stored bytes back. The 12345 row is the control: it round-tripped before
// the fix too, so a failure there is the harness, not the defect.
func TestMCPFieldsKeepNumberLiterals_Remote(t *testing.T) {
	s := storetest.NewSQLite(t)
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	owner, err := s.CreateUser(models.UserCreate{Email: "o@example.com", Name: "O", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "W", Slug: "w3217", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"n","type":"number"},{"key":"j","type":"json"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}

	m := mcpserver.NewMCPServer("t", "1", mcpserver.WithToolCapabilities(true))
	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return owner }}
	if _, err := RegisterCatalog(m, CatalogOptions{Doc: fieldsAliasDoc(t), Workspace: NewWorkspaceState(ws.Slug), Dispatcher: d, PadVersion: "test"}); err != nil {
		t.Fatal(err)
	}

	for i, tc := range []struct{ fields, want string }{
		{`{"n":12345}`, `"n":12345`},
		{`{"n":9007199254740993}`, `"n":9007199254740993`},
		{`{"n":-9007199254740993}`, `"n":-9007199254740993`},
		{`{"n":1.10}`, `"n":1.10`},
		{`{"n":0.1000000000000000055511151231257827}`, `"n":0.1000000000000000055511151231257827`},
		// A number NESTED in a json field value, which the float64 decode
		// rounded just the same.
		{`{"j":{"id":9007199254740993,"xs":[18014398509481985]}}`, `"j":{"id":9007199254740993,"xs":[18014398509481985]}`},
	} {
		args := fmt.Sprintf(`{"action":"update","workspace":%q,"ref":%q,"fields":%s}`, ws.Slug, item.Slug, tc.fields)
		resp := respText(t, m.HandleMessage(context.Background(), toolsCall(i, args)))
		if strings.Contains(resp, `"isError":true`) {
			t.Fatalf("%s: tool call failed: %s", tc.fields, resp)
		}
		got, err := s.GetItem(item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.Fields, tc.want) {
			t.Fatalf("%s: stored fields = %s, want it to contain %s", tc.fields, got.Fields, tc.want)
		}
	}
}

// TestMCPFieldsKeepNumberLiterals_Stdio covers the local door, which the
// filing believed unaffected: a `fields` OBJECT number is stringified into a
// --field entry, and the float64 it was stringified from had already been
// rounded. A `field: ["n=…"]` string entry was always safe; the object was not.
func TestMCPFieldsKeepNumberLiterals_Stdio(t *testing.T) {
	fd := &fakeDispatcher{}
	m := mcpserver.NewMCPServer("t", "1", mcpserver.WithToolCapabilities(true))
	if _, err := RegisterCatalog(m, CatalogOptions{Doc: fieldsAliasDoc(t), Workspace: NewWorkspaceState("ws"), Dispatcher: fd, PadVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct{ fields, want string }{
		{`{"n":12345}`, "n=12345"},
		{`{"n":9007199254740993}`, "n=9007199254740993"},
		{`{"n":1.10}`, "n=1.10"},
	} {
		fd.gotArgs = nil
		resp := respText(t, m.HandleMessage(context.Background(), toolsCall(i, `{"action":"update","ref":"TASK-1","fields":`+tc.fields+`}`)))
		if strings.Contains(resp, `"isError":true`) {
			t.Fatalf("%s: tool call failed: %s", tc.fields, resp)
		}
		if !containsString(fd.gotArgs, tc.want) {
			t.Fatalf("%s: dispatched args %q, want an entry %q", tc.fields, fd.gotArgs, tc.want)
		}
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestMCPFieldsNumberConflictCompare pins the conflict pass. A `fields`
// number and a `field` entry naming the same key are ONE write when they are
// the same number, however spelled — the answer they got while both were
// float64 — and a conflict when they are different numbers, including two
// that used to round to the same float64 and so compared equal.
func TestMCPFieldsNumberConflictCompare(t *testing.T) {
	fd := &fakeDispatcher{}
	m := mcpserver.NewMCPServer("t", "1", mcpserver.WithToolCapabilities(true))
	if _, err := RegisterCatalog(m, CatalogOptions{Doc: fieldsAliasDoc(t), Workspace: NewWorkspaceState("ws"), Dispatcher: fd, PadVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name, fields, entry string
		refused             bool
	}{
		{"same integer", `{"n":3}`, "n=3", false},
		{"3.0 is 3", `{"n":3.0}`, "n=3", false},
		{"1.10 is 1.1", `{"n":1.10}`, "n=1.1", false},
		{"different numbers", `{"n":4}`, "n=3", true},
		{"distinct above 2^53", `{"n":9007199254740993}`, "n=9007199254740992", true},
		{"not a number on the string side", `{"n":3}`, "n=three", true},
	} {
		args := fmt.Sprintf(`{"action":"update","ref":"TASK-1","fields":%s,"field":[%q]}`, tc.fields, tc.entry)
		resp := respText(t, m.HandleMessage(context.Background(), toolsCall(i, args)))
		if got := strings.Contains(resp, `"isError":true`); got != tc.refused {
			t.Errorf("%s: refused=%v, want %v: %s", tc.name, got, tc.refused, resp)
		}
	}
}
