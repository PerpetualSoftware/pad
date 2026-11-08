package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4278. The attachment remap now runs as a series of
// transactions; when it stops between them some items are rewritten and the
// rest are not. The import handler KEEPS that workspace, marks it partial and
// answers a 500 with the NON-retryable code import_partially_remapped (the
// server failed partway, so not a 400; and a retry would import the bundle a
// second time, so never database_busy or a Retry-After, even when the cause
// was a database bound). There is no route that reruns only the remap, so the
// text tells the importer plainly to delete the workspace and import again,
// and carries none of the store's error text (which can carry SQL).
func TestTASK4278_RemapStoppedBetweenChunks_SQLite(t *testing.T) {
	t.Parallel()
	remapStoppedBetweenChunks(t, store.DriverSQLite)
}

func TestTASK4278_RemapStoppedBetweenChunks_Postgres(t *testing.T) {
	t.Parallel()
	remapStoppedBetweenChunks(t, store.DriverPostgres)
}

func remapStoppedBetweenChunks(t *testing.T, driver store.DriverType) {
	dest := attachmentsServerOn(t, driver)
	remapCalls := 0
	dest.importBundleRemapFault = func() error {
		remapCalls++
		// The cause is a REAL statement-timeout error: IsDBBusy is true for
		// it, and the answer must still not be the retryable 503.
		cause := fmt.Errorf("update item x: SELECT secret FROM users: %w", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"})
		return &store.RemapPartialError{Chunks: 2, Items: 128, Cause: cause}
	}
	rr := postBundle(dest, "halfremapped", packInterchangeBundle(t, readInterchangeGolden(t)))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("a remap that stopped between chunks must be a 500, got %d %s", rr.Code, rr.Body.String())
	}
	if ra := rr.Header().Get("Retry-After"); ra != "" {
		t.Errorf("a non-retryable answer must carry no Retry-After, got %q", ra)
	}
	var env struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("not the error envelope: %v: %s", err, rr.Body.String())
	}
	if env.Error.Code != "import_partially_remapped" {
		t.Errorf("code = %q, want import_partially_remapped; body %s", env.Error.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"stopped partway", "delete this partial workspace and import the bundle again"} {
		if !strings.Contains(body, want) {
			t.Errorf("500 body does not say %q: %s", want, body)
		}
	}
	for _, leak := range []string{"SQLSTATE", "SELECT", "canceling statement", "update item", "database_busy", "busy"} {
		if strings.Contains(body, leak) {
			t.Errorf("500 body carries %q: %s", leak, body)
		}
	}
	if remapCalls != 1 {
		t.Errorf("the server ran the remap %d times for one request; nothing may retry it", remapCalls)
	}

	// The workspace is KEPT, and marked partial, with the category that names
	// the half-done remap (a sanitized note, nothing of the raw error).
	ws, err := dest.store.GetWorkspaceBySlug("halfremapped")
	if err != nil || ws == nil {
		t.Fatalf("the workspace should be kept: %v %v", ws, err)
	}
	st, err := dest.store.GetWorkspaceImportStatus(ws.ID)
	if err != nil || st == nil || st.Status != store.ImportStatusPartial {
		t.Fatalf("the kept workspace must carry the partial marker: %+v %v", st, err)
	}
	if !strings.Contains(st.Note, "only partly updated") || !strings.Contains(st.Note, "import again") {
		t.Errorf("partial note = %q, want the half-done-remap category", st.Note)
	}
	if strings.Contains(st.Note, "SQLSTATE") || strings.Contains(st.Note, "SELECT") {
		t.Errorf("partial note carries store text: %q", st.Note)
	}

	// Nothing in the server retried: one workspace exists. A SECOND identical
	// request is the caller's own choice, and only then is there a second.
	countWS := func() int {
		list, err := dest.store.ListWorkspaces()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, w := range list {
			if strings.HasPrefix(w.Slug, "halfremapped") {
				n++
			}
		}
		return n
	}
	if n := countWS(); n != 1 {
		t.Fatalf("after one request there must be exactly 1 workspace, got %d", n)
	}
	rr2 := postBundle(dest, "halfremapped", packInterchangeBundle(t, readInterchangeGolden(t)))
	if rr2.Code != http.StatusInternalServerError {
		t.Fatalf("second request: %d %s", rr2.Code, rr2.Body.String())
	}
	if n := countWS(); n != 2 || remapCalls != 2 {
		t.Errorf("a second request the caller chose to send makes the second workspace: workspaces=%d remapCalls=%d, want 2 and 2", n, remapCalls)
	}
}
