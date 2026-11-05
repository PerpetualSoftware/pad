package server

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4278: account deletion's time budget, counted from request
// arrival, and the session path's context. The Postgres tests run on a real
// Postgres (storetest.NewPostgres skips without PAD_TEST_POSTGRES_URL) and
// count rows with raw SQL.

// nginxProxyReadTimeout is the inner router's 60s proxy read timeout on its
// catch-all location. The test keeps the account-delete budget below it.
const nginxProxyReadTimeout = 60 * time.Second

func TestAccountDeleteBudgetBelowProxyTimeouts(t *testing.T) {
	if accountDeleteStmtTimeout >= accountDeleteTxBudget || accountDeleteLockTimeout >= accountDeleteTxBudget {
		t.Fatalf("statement %s / lock %s timeouts must stay below the %s request budget",
			accountDeleteStmtTimeout, accountDeleteLockTimeout, accountDeleteTxBudget)
	}
	if got := accountDeleteTxBudget + accountDeletePostCommitSlop; got >= nginxProxyReadTimeout {
		t.Fatalf("budget %s + post-commit allowance %s = %s must stay below nginx's %s proxy_read_timeout, "+
			"or the user sees a gateway error while the deletion completes behind it",
			accountDeleteTxBudget, accountDeletePostCommitSlop, got, nginxProxyReadTimeout)
	}
	srv := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if wt := srv.WriteTimeout; wt != 0 && wt <= accountDeleteTxBudget+accountDeletePostCommitSlop {
		t.Fatalf("http.Server.WriteTimeout %s cuts the connection before the %s budget + %s allowance",
			wt, accountDeleteTxBudget, accountDeletePostCommitSlop)
	}
}

func TestWriteBusy(t *testing.T) {
	for _, tc := range []struct {
		write bool
		err   error
		msg   string
	}{
		{true, &pgconn.PgError{Code: "57014"}, "The server is busy; nothing was saved. Please try again."},
		{false, &pgconn.PgError{Code: "57014"}, "The server is busy. Please try again."},
		// Cut off client-side: may have committed, so no "nothing was saved".
		{true, context.DeadlineExceeded, "The server was busy and may not have finished this request. Check before trying again."},
		{true, driver.ErrBadConn, "The server was busy and may not have finished this request. Check before trying again."},
		{false, context.DeadlineExceeded, "The server is busy. Please try again."},
	} {
		rr := httptest.NewRecorder()
		writeBusy(rr, tc.err, tc.write)
		// An outcome-unknown write tells the caller to check first, so it
		// carries no Retry-After; every other busy answer keeps "2".
		wantRA := "2"
		if tc.write && store.IsDBOutcomeUnknown(tc.err) {
			wantRA = ""
		}
		if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != wantRA {
			t.Fatalf("write=%v: %d Retry-After=%q, want %q", tc.write, rr.Code, rr.Header().Get("Retry-After"), wantRA)
		}
		var env struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Error.Code != "database_busy" || env.Error.Message != tc.msg {
			t.Fatalf("write=%v: body %s", tc.write, rr.Body.String())
		}
	}
}

// deleteAccountDirect runs handleDeleteAccount itself with a request whose
// recorded arrival is `ago` in the past (the middleware, which would stamp
// time.Now(), is bypassed so the arrival can be set).
func deleteAccountDirect(srv *Server, token string, arrivedAgo time.Duration) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"password": "correct-horse-battery-staple"})
	req := httptest.NewRequest("POST", "/api/v1/auth/delete-account", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.7:5555"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: token})
	ctx := context.WithValue(req.Context(), requestStartKey{}, time.Now().Add(-arrivedAgo))
	rr := httptest.NewRecorder()
	srv.handleDeleteAccount(rr, req.WithContext(ctx))
	return rr
}

type deleteAccountWorld struct {
	userID, token, wsID, itemID string
}

func newDeleteAccountWorld(t *testing.T, srv *Server, customerID string) deleteAccountWorld {
	t.Helper()
	userID, token := bootstrapAccountDeleteUser(t, srv, customerID)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Owned 4278", Slug: "owned-4278", OwnerID: userID})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, userID, "owner"); err != nil {
		t.Fatal(err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`})
	if err != nil {
		t.Fatal(err)
	}
	item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Authored", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(`UPDATE items SET created_by_user_id = $1 WHERE id = $2`, userID, item.ID); err != nil {
		t.Fatal(err)
	}
	return deleteAccountWorld{userID: userID, token: token, wsID: ws.ID, itemID: item.ID}
}

func (w deleteAccountWorld) assertIntact(t *testing.T, srv *Server) {
	t.Helper()
	var backend string
	if err := srv.store.DB().QueryRow(`SHOW server_version`).Scan(&backend); err != nil || backend == "" {
		t.Fatalf("not a real Postgres backend: %q %v", backend, err)
	}
	for _, c := range []struct{ what, q string }{
		{"users", `SELECT count(*) FROM users WHERE id = $1`},
		{"sessions", `SELECT count(*) FROM sessions WHERE user_id = $1`},
		{"memberships", `SELECT count(*) FROM workspace_members WHERE user_id = $1`},
		{"authored items", `SELECT count(*) FROM items WHERE created_by_user_id = $1`},
		{"live owned workspaces", `SELECT count(*) FROM workspaces WHERE owner_id = $1 AND deleted_at IS NULL`},
	} {
		var n int
		if err := srv.store.DB().QueryRow(c.q, w.userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n < 1 {
			t.Errorf("%s = %d after a busy refusal: the deletion was not rolled back", c.what, n)
		}
	}
}

func requireBusy503(t *testing.T, rr *httptest.ResponseRecorder, wantMsg string) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d (%s), want 503", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") != "2" {
		t.Errorf("Retry-After = %q", rr.Header().Get("Retry-After"))
	}
	var env struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "database_busy" || env.Error.Message != wantMsg {
		t.Fatalf("body %s, want database_busy %q", rr.Body.String(), wantMsg)
	}
}

// The deadline is counted from the request's ARRIVAL, not from when the
// transaction begins: a request that arrived 51 s ago has no budget left, and
// the deletion must not start; one that arrived 49.5 s ago has 0.5 s, and is
// refused when it cannot get its lock in that time.
func TestDeleteAccountDeadlineFromRequestArrival(t *testing.T) {
	t.Run("arrived 51s ago: refused, nothing changed", func(t *testing.T) {
		srv := accountDeleteServer(t, store.DriverPostgres)
		w := newDeleteAccountWorld(t, srv, "")
		rr := deleteAccountDirect(srv, w.token, 51*time.Second)
		requireBusy503(t, rr, busyWriteMessage)
		w.assertIntact(t, srv)
	})
	t.Run("arrived 49.5s ago and a lock held: 503 at about 0.5s", func(t *testing.T) {
		srv := accountDeleteServer(t, store.DriverPostgres)
		w := newDeleteAccountWorld(t, srv, "")
		holder, err := srv.store.DB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		timer := time.AfterFunc(8*time.Second, func() { _ = holder.Rollback() })
		defer timer.Stop()
		defer holder.Rollback()
		if _, err := holder.Exec(`SELECT 1 FROM items WHERE id = $1 FOR UPDATE`, w.itemID); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		rr := deleteAccountDirect(srv, w.token, 49500*time.Millisecond)
		elapsed := time.Since(start)
		requireBusy503(t, rr, busyWriteMessage)
		if elapsed > 4*time.Second {
			t.Fatalf("answered after %s; the 0.5s left on the budget was not honoured", elapsed)
		}
		holder.Rollback()
		w.assertIntact(t, srv)
	})
	t.Run("billing already cancelled: its own message", func(t *testing.T) {
		srv := accountDeleteServer(t, store.DriverPostgres)
		w := newDeleteAccountWorld(t, srv, "cus_busy")
		srv.SetCloudSidecar(&fakeSidecar{})
		rr := deleteAccountDirect(srv, w.token, 51*time.Second)
		requireBusy503(t, rr, "Billing was already cancelled; try again to finish deleting your account")
		w.assertIntact(t, srv)
	})
}

// The session middleware answers a request whose context is already done with
// nothing at all: no 500 body, no error log, and the query is not run.
func TestSessionAuth_CancelledContextIsQuiet(t *testing.T) {
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "quiet@test.com", "Quiet")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Body.Len() != 0 {
		t.Fatalf("a cancelled request was answered: %d %s", rr.Code, rr.Body.String())
	}
}
