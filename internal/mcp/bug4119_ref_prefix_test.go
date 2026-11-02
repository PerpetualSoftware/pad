package mcp

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	padserver "github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestBUG4119_RemoteWrongPrefix(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, exists := range []bool{false, true} {
			for _, action := range []string{"comment", "delete", "update"} {
				t.Run(fmt.Sprintf("pg=%t/ident=%t/%s", pg, exists, action), func(t *testing.T) {
					f := storetest.NewRefPrefixFixture(t, pg, exists)
					s := f.Store
					srv := padserver.New(s)
					t.Cleanup(srv.Stop)
					u, err := s.CreateUser(models.UserCreate{Email: "mcp-prefix@example.com", Name: "Owner", Password: "test-password-long-enough"})
					if err != nil {
						t.Fatal(err)
					}
					if err = s.AddWorkspaceMember(f.Workspace.ID, u.ID, "owner"); err != nil {
						t.Fatal(err)
					}
					d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return u }}
					ms := NewServer(Options{Version: "test"})
					if _, err = RegisterCatalog(ms.MCP(), CatalogOptions{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(f.Workspace.Slug), Dispatcher: d, PadVersion: "test"}); err != nil {
						t.Fatal(err)
					}
					transport := NewRemoteTransport(ms.MCP(), bug2772Sessions{})
					ts := httptest.NewServer(transport)
					t.Cleanup(ts.Close)
					w := bug2772World{t: t, url: ts.URL + "/mcp"}
					session := w.initialize("prefix-test")
					before := f.Snapshot(t)
					extra := ""
					if action == "comment" {
						extra = `,"message":"Wrong comment"`
					}
					if action == "update" {
						extra = `,"title":"Wrong title"`
					}
					args := fmt.Sprintf(`{"action":%q,"workspace":%q,"ref":%q%s}`, action, f.Workspace.Slug, f.WrongRef, extra)
					raw := w.call("pad_item", args, session)
					if !strings.Contains(raw, `"isError":true`) {
						t.Errorf("remote MCP %s wrong-prefix: want tool error, got %s", action, raw)
					}
					if !strings.Contains(strings.ToLower(raw), "not_found") && !strings.Contains(strings.ToLower(raw), "not found") {
						t.Errorf("remote MCP must refuse as not-found: %s", raw)
					}
					f.AssertUnchanged(t, before)
				})
			}
		}
	}
}
