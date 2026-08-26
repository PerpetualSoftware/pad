package mcp

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestZZProbeMCPSeam(t *testing.T) {
	var s = storetest.NewSQLite(t)
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		s = storetest.NewPostgres(t)
	}
	srv := server.New(s)
	t.Cleanup(srv.Stop)
	fmt.Printf("DRIVER: %s\n", s.D().Driver())

	owner, err := s.CreateUser(models.UserCreate{Email: "o@example.com", Name: "O", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Seam", Slug: "seam-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	}); err != nil {
		t.Fatal(err)
	}

	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: func(context.Context) *models.User { return owner }}

	for _, tc := range []struct{ name, ref, wsSlug string }{
		{"ref with raw 0xff", "bad-\xff-x", "seam-ws"},
		{"ref with NUL", "bad-\x00-x", "seam-ws"},
		{"ref with truncated seq", "bad-\xc3(-x", "seam-ws"},
		{"workspace slug with raw 0xff", "TASK-1", "bad-\xff-ws"},
		{"workspace slug with NUL", "TASK-1", "bad-\x00-ws"},
		{"clean control (absent ref)", "TASK-999", "seam-ws"},
	} {
		ctx := WithDispatchInput(context.Background(), map[string]any{
			"ref": tc.ref, "workspace": tc.wsSlug,
		})
		res, err := d.Dispatch(ctx, []string{"item", "show"}, nil)
		out := ""
		if res != nil {
			out = textOf(res)
		}
		if len(out) > 130 {
			out = out[:130]
		}
		fmt.Printf("%-32s err=%v isError=%v\n     %s\n", tc.name, err, res != nil && res.IsError, out)
	}
}
