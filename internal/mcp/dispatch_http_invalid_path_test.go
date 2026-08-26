package mcp

// BUG-2782, the seam. The remote /mcp transport does NOT reach the server
// over a socket: HTTPHandlerDispatcher SYNTHESIZES an *http.Request with
// buildAuthedRequest and calls Handler.ServeHTTP in-process. So "the path
// middleware covers every route" is a claim about a door this transport
// walks past, and it is worth driving rather than reasoning about — the
// seam between two independently-correct designs is where the third bug
// lives.
//
// It is covered, for a reason nothing else in the tree states: Handler is
// the *server.Server, chi's Mux.ServeHTTP runs mx.handler (middlewares +
// routeHTTP) on BOTH of its branches, and buildAuthedRequest deliberately
// forces the fresh-routing branch with a typed-nil RouteCtxKey. Root
// middleware therefore runs for a synthesized request exactly as for a
// socket one. That chain is load-bearing and unstated; this test is what
// notices if any link in it changes.
//
// The behaviour it pins is not just "no 500". An MCP tool argument is a
// JSON string, so an agent can put a raw invalid byte — or a JSON escape
// naming U+0000 — into a ref or a workspace slug. Fixed, that is
// validation_failed on both backends: the agent is told its INPUT is
// wrong, which is true and actionable.
//
// Unfixed, the two backends answered DIFFERENTLY, and only one of the two
// answers is dangerous — stated per backend because the tidier single
// sentence ("unfixed this was upstream_error") is false half the time, and
// this file runs on whichever backend the suite was started with:
//
//   - Postgres: upstream_error, whose hint tells the agent the failure is
//     "usually transient, retry" — for an input that can never succeed. An
//     agent obeying that hint retries forever. Same misclassification
//     family as the retry-hostile code BUG-2675 added.
//   - SQLite: item_not_found / unknown_workspace — wrong, since the ref is
//     not merely absent but unaskable, though harmless to an agent.
//
// Both are wrong and the fix replaces both, so the assertions below hold on
// either backend; the upstream_error leg is the one that only reproduces
// under PAD_TEST_POSTGRES_URL, which is why the fixture takes Postgres when
// it is available rather than pinning itself to SQLite.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func newInvalidPathSeamFixture(t *testing.T) (*HTTPHandlerDispatcher, string) {
	t.Helper()
	s := storetest.NewSQLite(t)
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		// Under `make test-pg` this exercises the dangerous half — the
		// retry-hostile upstream_error the fix removes. On SQLite the same
		// assertions still discriminate, against the milder wrong answer.
		s = storetest.NewPostgres(t)
	}
	srv := server.New(s)
	t.Cleanup(srv.Stop)

	owner, err := s.CreateUser(models.UserCreate{
		Email: "seam-owner@example.com", Name: "Seam Owner",
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Seam WS", Slug: "seam-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if _, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`,
	}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	return &HTTPHandlerDispatcher{
		Handler:      srv,
		UserResolver: func(context.Context) *models.User { return owner },
	}, ws.Slug
}

func TestHTTPDispatchInvalidPathTextIsAValidationError(t *testing.T) {
	d, ws := newInvalidPathSeamFixture(t)

	cases := []struct{ name, ref, workspace string }{
		{"ref carries a raw invalid byte", "bad-\xff-x", ws},
		{"ref carries a NUL", "bad-\x00-x", ws},
		{"ref carries a truncated UTF-8 sequence", "bad-\xc3(-x", ws},
		{"workspace slug carries a raw invalid byte", "TASK-1", "bad-\xff-ws"},
		{"workspace slug carries a NUL", "TASK-1", "bad-\x00-ws"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithDispatchInput(context.Background(), map[string]any{
				"ref": tc.ref, "workspace": tc.workspace,
			})
			res, err := d.Dispatch(ctx, []string{"item", "show"}, nil)
			if err != nil {
				t.Fatalf("Dispatch errored at transport: %v", err)
			}
			if res == nil || !res.IsError {
				t.Fatalf("expected a refusal, got %+v", res)
			}
			body := textOf(res)
			if !strings.Contains(body, "validation_failed") {
				t.Fatalf("expected validation_failed, got: %s", body)
			}
			// The specific thing that must NOT come back. Unfixed, this is
			// upstream_error, whose hint says the failure is usually
			// transient and worth retrying — advice that is false for an
			// input that can never succeed.
			if strings.Contains(body, "upstream_error") || strings.Contains(body, "500") {
				t.Fatalf("invalid path text reported as an upstream/transient failure: %s", body)
			}
		})
	}

	// Control: a well-formed ref that simply does not exist must still get
	// the ordinary not-found answer. Without this leg a dispatcher that
	// refused EVERY ref would pass every assertion above.
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"ref": "TASK-999", "workspace": ws,
	})
	res, err := d.Dispatch(ctx, []string{"item", "show"}, nil)
	if err != nil {
		t.Fatalf("control Dispatch errored at transport: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("control: expected a not-found refusal, got %+v", res)
	}
	if body := textOf(res); !strings.Contains(body, "item_not_found") {
		t.Fatalf("control: expected item_not_found for a valid-but-absent ref, got: %s", body)
	}
}
