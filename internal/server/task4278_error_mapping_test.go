package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// TASK-4278: no database bound reaches a caller as a bare 500, and no
// store error text reaches a body.

// --- the guard ---------------------------------------------------------------

// no500ExemptFuncs are the functions that ARE the mapping: they take the
// status as data or are the single funnel that decides it.
var no500ExemptFuncs = map[string]bool{
	"writeInternalError":          true, // the generic funnel; busy is mapped first
	"storeItemCreateError":        true, // carries the store error in cause; write() maps busy
	"writeStoreErrorNotRetryable": true, // the deliberate non-retryable 500: busy must NOT become a retryable 503 (import_partially_remapped)
}

// no500AllowList is the reviewed list of 500 sites that carry neither a
// `// 500:` justification nor an exempt function. It is empty on purpose: every
// kept 500 says why on the line above it. Add an entry here only with a
// reviewer's agreement, as "file.go:function".
var no500AllowList = map[string]string{}

func TestNo500DiscardsStoreError(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var bad []string
	seen := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		// Lines carrying a `// 500:` justification.
		just := map[int]bool{}
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(c.Text, "//")), "500:") {
					just[fset.Position(c.Pos()).Line] = true
				}
			}
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			// Selectors passed straight to the mapping helpers are the
			// conversion itself: the helper decides busy-or-500.
			viaHelper := map[ast.Node]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && strings.HasPrefix(id.Name, "writeStoreError") {
						for _, a := range call.Args {
							viaHelper[a] = true
						}
					}
				}
				return true
			})
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				// A 500 call that publishes err.Error() leaks store text.
				if call, ok := n.(*ast.CallExpr); ok && callHas500(call) {
					for _, a := range call.Args {
						ast.Inspect(a, func(m ast.Node) bool {
							if c, ok := m.(*ast.CallExpr); ok {
								if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" && len(c.Args) == 0 {
									bad = append(bad, fmt.Sprintf("%s:%d: a 500 body is built from .Error()", name, fset.Position(c.Pos()).Line))
								}
							}
							return true
						})
					}
				}
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "StatusInternalServerError" || viaHelper[sel] {
					return true
				}
				seen++
				line := fset.Position(sel.Pos()).Line
				if no500ExemptFuncs[fd.Name.Name] || just[line] || just[line-1] || just[line-2] || no500AllowList[name+":"+fd.Name.Name] != "" {
					return true
				}
				bad = append(bad, fmt.Sprintf("%s:%d (%s): http.StatusInternalServerError with no `// 500: <why not a store error>` on the line above; "+
					"route store errors through writeStoreErrorR / writeStoreError / writeInternalError so a busy database answers 503 database_busy",
					name, line, fd.Name.Name))
				return true
			})
		}
	}
	if seen < 20 {
		t.Fatalf("scan saw only %d StatusInternalServerError uses; the scan is broken", seen)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

func callHas500(call *ast.CallExpr) bool {
	for _, a := range call.Args {
		if sel, ok := a.(*ast.SelectorExpr); ok && sel.Sel.Name == "StatusInternalServerError" {
			return true
		}
	}
	return false
}

// --- helpers -----------------------------------------------------------------

type busyEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeBusy(t *testing.T, rr *httptest.ResponseRecorder) busyEnvelope {
	t.Helper()
	var env busyEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the error envelope: %v: %s", err, rr.Body.String())
	}
	return env
}

// assertBusy checks the shared 503 shape and that nothing of the database
// reached the body.
func assertBusy(t *testing.T, rr *httptest.ResponseRecorder, wantWrite bool) {
	t.Helper()
	assertBusyWording(t, rr, wantWrite, "")
}

// assertBusyUnknown is assertBusy for a client-side interruption of a WRITE
// (store.IsDBOutcomeUnknown): same status and code, NO Retry-After (the text
// says to check first), and the wording must not claim nothing was saved.
func assertBusyUnknown(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	assertBusyWording(t, rr, true, busyUnknownWriteMessage)
	if ra := rr.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("an outcome-unknown write carries Retry-After %q, inviting the retry its text forbids", ra)
	}
	if strings.Contains(rr.Body.String(), "nothing was saved") {
		t.Fatalf("an outcome-unknown write claims nothing was saved: %s", rr.Body.String())
	}
}

func assertBusyWording(t *testing.T, rr *httptest.ResponseRecorder, wantWrite bool, unknownOverride string) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" && unknownOverride == "" {
		t.Fatalf("no Retry-After on a busy answer")
	}
	env := decodeBusy(t, rr)
	if env.Error.Code != "database_busy" {
		t.Fatalf("code = %q, want database_busy; body %s", env.Error.Code, rr.Body.String())
	}
	wantMsg := busyReadMessage
	if wantWrite {
		wantMsg = busyWriteMessage
	}
	if unknownOverride != "" {
		wantMsg = unknownOverride
	}
	if env.Error.Message != wantMsg {
		t.Fatalf("message = %q, want %q", env.Error.Message, wantMsg)
	}
	body := strings.ToLower(rr.Body.String())
	for _, leak := range []string{"sqlstate", "57014", "55p03", "25p03", "57p01", "select ", "insert ", "update ", "platform_settings", "pgx", "timeout"} {
		if strings.Contains(body, leak) {
			t.Fatalf("body leaks %q: %s", leak, rr.Body.String())
		}
	}
}

func adminRequest(method, target, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	user := &models.User{ID: "u-admin", Role: "admin"}
	return req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, user))
}

// lockPlatformSettings holds ACCESS EXCLUSIVE on platform_settings from a
// separate pooled connection until the returned release is called, so any
// handler statement touching the table waits on it.
func lockPlatformSettings(t *testing.T, st *store.Store) (release func()) {
	t.Helper()
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	if _, err := tx.Exec("LOCK TABLE platform_settings IN ACCESS EXCLUSIVE MODE"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("lock platform_settings: %v", err)
	}
	var once bool
	return func() {
		if !once {
			once = true
			_ = tx.Rollback()
		}
	}
}

// --- handler-level mapping, real Postgres SQLSTATEs ---------------------------

func busyMappingCases(t *testing.T, st *store.Store, wantCode string) {
	t.Helper()
	srv := New(st)
	release := lockPlatformSettings(t, st)
	defer release()

	// A READ (GET settings): busy wording, no "nothing was saved".
	start := time.Now()
	rr := httptest.NewRecorder()
	srv.handleGetPlatformSettings(rr, adminRequest("GET", "/api/v1/admin/settings", ""))
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the bound did not fire: handler took %s", time.Since(start))
	}
	assertBusy(t, rr, false)
	if strings.Contains(rr.Body.String(), "nothing was saved") {
		t.Fatalf("a read must not claim a write rolled back: %s", rr.Body.String())
	}

	// A WRITE (PUT settings): write wording, nothing saved.
	rr = httptest.NewRecorder()
	srv.handleUpdatePlatformSettings(rr, adminRequest("PUT", "/api/v1/admin/settings", `{"email_provider":"maileroo"}`))
	assertBusy(t, rr, true)

	// The error underneath really carries the SQLSTATE the case is named for.
	release()
	release2 := lockPlatformSettings(t, st)
	defer release2()
	_, err := st.GetPlatformSettings()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != wantCode {
		t.Fatalf("store error = %v, want SQLSTATE %s", err, wantCode)
	}
	if !store.IsDBBusy(err) {
		t.Fatalf("IsDBBusy(%v) = false", err)
	}
}

func TestBusyMapping_57014(t *testing.T) {
	st := storetest.NewPostgresBounded(t, store.RuntimeLimits{Statement: 400 * time.Millisecond})
	busyMappingCases(t, st, "57014")
}

func TestBusyMapping_55P03(t *testing.T) {
	st := storetest.NewPostgresBounded(t, store.RuntimeLimits{Lock: 400 * time.Millisecond})
	busyMappingCases(t, st, "55P03")
}

// A session idle in a transaction past idle_in_transaction_session_timeout is
// terminated (25P03); the next statement on it returns that error (or a dead
// connection), and both the funnel and the request-aware helper answer it as
// busy, never as a 500.
func TestBusyMapping_IdleTermination(t *testing.T) {
	st := storetest.NewPostgresBounded(t, store.RuntimeLimits{IdleInTx: 300 * time.Millisecond})
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond)
	_, err = tx.Exec("SELECT 1")
	if err == nil {
		t.Fatal("the idle transaction was not terminated")
	}
	if !store.IsDBBusy(err) {
		t.Fatalf("IsDBBusy(%v) = false; an idle termination must be busy", err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code != "25P03" && pgErr.Code != "57P01" {
		t.Fatalf("SQLSTATE %s, want 25P03 or 57P01", pgErr.Code)
	}

	for _, method := range []string{"GET", "POST"} {
		rr := httptest.NewRecorder()
		writeStoreErrorR(rr, httptest.NewRequest(method, "/x", nil), err, http.StatusInternalServerError, "internal_error", "Failed to do it")
		assertBusy(t, rr, method == "POST")
	}
	rr := httptest.NewRecorder()
	writeInternalError(rr, err)
	assertBusy(t, rr, false)
}

// --- unit: the helpers --------------------------------------------------------

func TestWriteStoreErrorR_Shapes(t *testing.T) {
	t.Parallel()
	plain := errors.New(`pq: relation "items" does not exist (SQLSTATE 42P01)`)
	rr := httptest.NewRecorder()
	writeStoreErrorR(rr, httptest.NewRequest("GET", "/x", nil), plain, http.StatusInternalServerError, "internal_error", "Failed to load it")
	if rr.Code != 500 || decodeBusy(t, rr).Error.Message != "Failed to load it" || strings.Contains(rr.Body.String(), "SQLSTATE") {
		t.Fatalf("non-busy error: %d %s", rr.Code, rr.Body.String())
	}

	deadline := fmt.Errorf("load: %w", context.DeadlineExceeded)
	rr = httptest.NewRecorder()
	writeStoreErrorR(rr, httptest.NewRequest("PATCH", "/x", nil), deadline, http.StatusInternalServerError, "internal_error", "x")
	assertBusyUnknown(t, rr)
	// ... and a READ cut off the same way keeps the read wording.
	rr = httptest.NewRecorder()
	writeStoreErrorR(rr, httptest.NewRequest("GET", "/x", nil), deadline, http.StatusInternalServerError, "internal_error", "x")
	assertBusy(t, rr, false)
	// A definitive (server-signalled) write still says nothing was saved.
	rr = httptest.NewRecorder()
	writeStoreErrorR(rr, httptest.NewRequest("PATCH", "/x", nil), &pgconn.PgError{Code: "57014"}, http.StatusInternalServerError, "internal_error", "x")
	assertBusy(t, rr, true)

	// A client that left gets no body, but the recorded status is a 503, never
	// an implicit 200 (TASK-4278).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rr = httptest.NewRecorder()
	writeStoreErrorR(rr, httptest.NewRequest("GET", "/x", nil).WithContext(ctx), context.Canceled, http.StatusInternalServerError, "internal_error", "x")
	if rr.Body.Len() != 0 {
		t.Fatalf("a cancelled request was answered: %s", rr.Body.String())
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("a cancelled request's recorded status = %d, want 503 (not an implicit 200)", rr.Code)
	}

	// The busy-as wording: a step after the main write committed.
	rr = httptest.NewRecorder()
	writeStoreErrorBusyAs(rr, httptest.NewRequest("POST", "/x", nil), deadline, http.StatusInternalServerError, "internal_error", "x", "custom wording")
	if rr.Code != 503 || decodeBusy(t, rr).Error.Message != "custom wording" {
		t.Fatalf("busy-as: %d %s", rr.Code, rr.Body.String())
	}
}

func TestItemCreateError_BusyCauseIsMapped(t *testing.T) {
	t.Parallel()
	busy := fmt.Errorf("insert: %w", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"})
	rr := httptest.NewRecorder()
	storeItemCreateError(busy, "An internal error occurred").write(rr, httptest.NewRequest("POST", "/x", nil))
	assertBusy(t, rr, true)

	rr = httptest.NewRecorder()
	storeItemCreateError(errors.New("boom"), "An internal error occurred").write(rr, httptest.NewRequest("POST", "/x", nil))
	if rr.Code != 500 || strings.Contains(rr.Body.String(), "boom") {
		t.Fatalf("plain cause: %d %s", rr.Code, rr.Body.String())
	}
}

// BUG-2778 reconciled: a busy rename is database_busy now; a cascade that ran
// out of retries keeps its own lock_contention code.
func TestDocumentRename_BusyClassification(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"55P03", "40P01", "57014"} {
		if !store.IsDBBusy(fmt.Errorf("rename: %w", &pgconn.PgError{Code: code})) {
			t.Errorf("SQLSTATE %s must be busy", code)
		}
	}
	if store.IsDBBusy(&pgconn.PgError{Code: "23505"}) {
		t.Error("a unique violation is not busy")
	}
	if store.IsDBBusy(store.ErrLinkCascadeContention) {
		t.Error("ErrLinkCascadeContention is not a database bound; it keeps its own code")
	}
}
