package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4278. ANY failure of the import's attachment remap keeps
// the workspace, marks it partial and answers the non-retryable 500, through
// one helper, with plain text: the first chunk's failure (a plain store error,
// nothing committed, possibly a raw 57014/55P03) as well as a stop between
// chunks, on the main path AND on the deferred retry that runs after a
// keep-path error. A busy cause is never database_busy for an import: a retry
// would create a second workspace.

func assertNonRetryableRemapAnswer(t *testing.T, srv *Server, rr *httptest.ResponseRecorder, slug, wantCode string, extraWant ...string) {
	t.Helper()
	code, body := rr.Code, rr.Body.String()
	if code != http.StatusInternalServerError {
		t.Fatalf("a failed remap must answer 500, got %d %s", code, body)
	}
	if ra := rr.Header().Get("Retry-After"); ra != "" {
		t.Errorf("a non-retryable answer carries no Retry-After, got %q", ra)
	}
	var env struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("not the error envelope: %v: %s", err, body)
	}
	if env.Error.Code != wantCode {
		t.Errorf("code = %q, want %q; body %s", env.Error.Code, wantCode, body)
	}
	want := append([]string{"delete this partial workspace and import the bundle again"}, extraWant...)
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("body does not say %q: %s", w, body)
		}
	}
	for _, leak := range []string{"SQLSTATE", "57014", "55P03", "SELECT", "canceling statement", "scan items", "database_busy", "busy", "pgx", "users"} {
		if strings.Contains(body, leak) {
			t.Errorf("body carries %q: %s", leak, body)
		}
	}
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("the workspace should be kept: %v %v", ws, err)
	}
	st, err := srv.store.GetWorkspaceImportStatus(ws.ID)
	if err != nil || st == nil || st.Status != store.ImportStatusPartial {
		t.Fatalf("the kept workspace must carry the partial marker: %+v %v", st, err)
	}
	if strings.Contains(st.Note, "SQLSTATE") || strings.Contains(st.Note, "SELECT") || strings.Contains(st.Note, "57014") {
		t.Errorf("partial note carries store text: %q", st.Note)
	}
}

func TestTASK4278_RemapFirstChunkFailure_SQLite(t *testing.T) {
	t.Parallel()
	remapFirstChunkFailure(t, store.DriverSQLite)
}

func TestTASK4278_RemapFirstChunkFailure_Postgres(t *testing.T) {
	t.Parallel()
	remapFirstChunkFailure(t, store.DriverPostgres)
}

func remapFirstChunkFailure(t *testing.T, driver store.DriverType) {
	for name, cause := range map[string]error{
		// A real pgconn 57014 under a store wrapper that names SQL.
		"pgconn 57014": fmt.Errorf("scan items for remap: SELECT id, content FROM items: %w",
			&pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}),
		// Raw driver text with no typed error behind it.
		"raw text": errors.New("scan items for remap: ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)"),
	} {
		t.Run(name, func(t *testing.T) {
			dest := attachmentsServerOn(t, driver)
			calls := 0
			dest.importBundleRemapFault = func() error { calls++; return cause }
			rr := postBundle(dest, "firstchunk", packInterchangeBundle(t, readInterchangeGolden(t)))
			assertNonRetryableRemapAnswer(t, dest, rr, "firstchunk", "import_remap_failed")
			if calls != 1 {
				t.Errorf("the remap ran %d times for one request; nothing may retry it", calls)
			}
		})
	}
}

func TestTASK4278_DeferredRemapFailureOnKeptWorkspace_SQLite(t *testing.T) {
	t.Parallel()
	deferredRemapFailure(t, store.DriverSQLite)
}

func TestTASK4278_DeferredRemapFailureOnKeptWorkspace_Postgres(t *testing.T) {
	t.Parallel()
	deferredRemapFailure(t, store.DriverPostgres)
}

// The deferred retry runs after a keep-path error (here: an oversized blob).
// Its remap failure rides next to that primary error; the answer is the
// non-retryable 500, not the 400 import_failed the primary error alone gets.
func deferredRemapFailure(t *testing.T, driver store.DriverType) {
	cases := []struct {
		name     string
		cause    error
		wantCode string
		want     string
	}{
		{"stopped between chunks",
			&store.RemapPartialError{Chunks: 2, Items: 128, Cause: fmt.Errorf("SELECT secret FROM users: %w", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"})},
			"import_partially_remapped", "stopped partway"},
		{"first chunk",
			fmt.Errorf("scan items for remap: %w", &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}),
			"import_remap_failed", "could not be updated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := attachmentsServerOn(t, driver)
			dest.attachmentMaxBytes = 1024 // the golden's blob fits, the added one does not
			calls := 0
			dest.importBundleRemapFault = func() error { calls++; return tc.cause }
			rr := postBundle(dest, "deferredremap", packInterchangeBundle(t, withOversizedSecondAttachment(t, readInterchangeGolden(t))))
			// The primary keep-path error (the oversized blob) is said only as
			// a fixed category, never in its own words.
			assertNonRetryableRemapAnswer(t, dest, rr, "deferredremap", tc.wantCode, "the import also failed earlier (an attachment file could not be read", tc.want)
			if strings.Contains(rr.Body.String(), "exceeds") {
				t.Errorf("the primary error's own text reached the body: %s", rr.Body.String())
			}
			if calls != 1 {
				t.Errorf("the deferred remap ran %d times; want 1", calls)
			}
		})
	}
}
