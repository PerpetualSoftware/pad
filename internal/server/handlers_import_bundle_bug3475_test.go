package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3475: a TRANSPORT failure (a stalled upload, a reset, a client that
// went away) rolls the partial workspace back instead of keeping it, and
// every keyed attempt's outcome can be asked for afterwards, because the
// response that would have said it may never arrive.

// assertRolledBack: the 400 says the upload was interrupted and carries the
// cause, and no workspace is left live or in the importer's list.
func assertRolledBack(t *testing.T, srv *Server, userID, slug string, rr *httptest.ResponseRecorder, wantCause string) {
	t.Helper()
	body := rr.Body.String()
	if rr.Code != http.StatusBadRequest || !strings.Contains(body, "import_interrupted") {
		t.Fatalf("want 400 import_interrupted, got %d: %s", rr.Code, body)
	}
	if !strings.Contains(body, wantCause) {
		t.Errorf("the 400 should carry the cause %q: %s", wantCause, body)
	}
	if live, err := srv.store.GetWorkspaceBySlug(slug); err != nil || live != nil {
		t.Fatalf("workspace %q is still live after an interrupted upload (err %v)", slug, err)
	}
	mine, err := srv.store.GetUserWorkspaces(userID)
	if err != nil {
		t.Fatalf("GetUserWorkspaces: %v", err)
	}
	for _, w := range mine {
		if w.Slug == slug {
			t.Fatalf("workspace %q is in the importer's list after an interrupted upload", slug)
		}
	}
}

func importKeyed(srv *Server, name, key string, body io.Reader, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name="+name+"&import_key="+key, body)
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "192.0.2.1:1234"
	const csrf = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: token})
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: csrf})
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func importStatus(t *testing.T, srv *Server, key, token string) (int, importOutcome) {
	t.Helper()
	rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/import-status?key="+key, nil, token)
	var out importOutcome
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode status: %v: %s", err, rr.Body.String())
		}
	}
	return rr.Code, out
}

func TestImportBundle_BUG3475_Outcomes_SQLite(t *testing.T) {
	importOutcomes3475(t, store.DriverSQLite)
}

func TestImportBundle_BUG3475_Outcomes_Postgres(t *testing.T) {
	importOutcomes3475(t, store.DriverPostgres)
}

func importOutcomes3475(t *testing.T, driver store.DriverType) {
	full := realBundleWithBlob(t)
	prefix, rest := splitAfterManifest(t, full)

	t.Run("complete", func(t *testing.T) {
		srv := attachmentsServerOn(t, driver)
		u, tok := memberImporter(t, srv)
		rr := importKeyed(srv, "whole", "key-complete-1", bytes.NewReader(full), tok)
		if rr.Code != http.StatusCreated {
			t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
		}
		code, st := importStatus(t, srv, "key-complete-1", tok)
		if code != http.StatusOK || st.State != importStateComplete || st.WorkspaceSlug != "whole" || st.OwnerUsername != u.Username {
			t.Fatalf("status = %d %+v, want complete naming whole and %q", code, st, u.Username)
		}
	})

	t.Run("interrupted after the workspace was created: removed", func(t *testing.T) {
		srv := attachmentsServerOn(t, driver)
		u, tok := memberImporter(t, srv)
		rr := importKeyed(srv, "cutoff", "key-cutoff-01", &failingAfter{data: bytes.NewReader(prefix), err: io.ErrUnexpectedEOF}, tok)
		assertRolledBack(t, srv, u.ID, "cutoff", rr, "unexpected EOF")
		if !strings.Contains(rr.Body.String(), "partial workspace was removed") {
			t.Errorf("the 400 should say the partial workspace was removed: %s", rr.Body.String())
		}
		code, st := importStatus(t, srv, "key-cutoff-01", tok)
		if code != http.StatusOK || st.State != importStateRemoved || st.WorkspaceSlug != "" {
			t.Fatalf("status = %d %+v, want removed with no workspace named", code, st)
		}
	})

	t.Run("interrupted before the workspace was created: not created", func(t *testing.T) {
		srv := attachmentsServerOn(t, driver)
		u, tok := memberImporter(t, srv)
		rr := importKeyed(srv, "early", "key-early-001", &failingAfter{data: bytes.NewReader(prefix[:64]), err: io.ErrUnexpectedEOF}, tok)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "import_interrupted") {
			t.Fatalf("want 400 import_interrupted, got %d: %s", rr.Code, rr.Body.String())
		}
		if live, _ := srv.store.GetWorkspaceBySlug("early"); live != nil {
			t.Fatal("a workspace exists after an upload cut off inside pad-export.json")
		}
		code, st := importStatus(t, srv, "key-early-001", tok)
		if code != http.StatusOK || st.State != importStateNotCreated {
			t.Fatalf("status = %d %+v, want not_created", code, st)
		}
		_ = u
	})

	// The other door, which Q1 keeps: a DATA error mid-stream reaches a live
	// client, so the partial workspace is kept and named.
	t.Run("data error after the workspace was created: kept", func(t *testing.T) {
		srv := attachmentsServerOn(t, driver)
		u, tok := memberImporter(t, srv)
		bad := append([]byte(nil), rest...)
		for i := range bad {
			bad[i] ^= 0xA5
		}
		rr := importKeyed(srv, "corrupted", "key-corrupt-1", bytes.NewReader(append(append([]byte(nil), prefix...), bad...)), tok)
		assertKeptThroughTheDoor(t, srv, u.ID, tok, "corrupted", rr, "read tar entry")
		code, st := importStatus(t, srv, "key-corrupt-1", tok)
		if code != http.StatusOK || st.State != importStateKept || st.WorkspaceSlug != "corrupted" {
			t.Fatalf("status = %d %+v, want kept naming corrupted", code, st)
		}
	})

	t.Run("not a gzip at all: not created", func(t *testing.T) {
		srv := attachmentsServerOn(t, driver)
		_, tok := memberImporter(t, srv)
		rr := importKeyed(srv, "garbage", "key-garbage-1", strings.NewReader("this is not gzip"), tok)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "bad_bundle") {
			t.Fatalf("want 400 bad_bundle, got %d: %s", rr.Code, rr.Body.String())
		}
		if code, st := importStatus(t, srv, "key-garbage-1", tok); code != http.StatusOK || st.State != importStateNotCreated {
			t.Fatalf("status = %d %+v, want not_created", code, st)
		}
	})
}

// The status door answers only the caller who started the attempt, and only
// for a well-formed key; a malformed key on the import itself is refused
// before any of the body is read.
func TestImportStatus_BUG3475_Refusals(t *testing.T) {
	full := realBundleWithBlob(t)
	srv := attachmentsServerOn(t, store.DriverSQLite)
	_, tokA := memberImporter(t, srv)
	other := realUser(t, srv, "other@pad.test")
	tokB, err := srv.store.CreateSession(other.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if rr := importKeyed(srv, "mine", "key-owned-by-a", bytes.NewReader(full), tokA); rr.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	if code, _ := importStatus(t, srv, "key-owned-by-a", tokB); code != http.StatusNotFound {
		t.Errorf("another user's key answered %d, want 404", code)
	}
	if code, _ := importStatus(t, srv, "key-never-used", tokA); code != http.StatusNotFound {
		t.Errorf("an unknown key answered %d, want 404", code)
	}
	if code, _ := importStatus(t, srv, "bad.key", tokA); code != http.StatusBadRequest {
		t.Errorf("a malformed key answered %d, want 400", code)
	}

	body := &countingReader{r: bytes.NewReader(full)}
	rr := importKeyed(srv, "badkey", "short", body, tokA)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "import_key") {
		t.Fatalf("a malformed import_key answered %d: %s", rr.Code, rr.Body.String())
	}
	if body.n != 0 {
		t.Errorf("the import read %d body bytes before refusing a malformed key", body.n)
	}
	if live, _ := srv.store.GetWorkspaceBySlug("badkey"); live != nil {
		t.Error("a workspace was created under a malformed key")
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestImportOutcomeRegistry_BUG3475(t *testing.T) {
	if importOutcomeRunningTTL <= defaultImportReadCeiling {
		t.Fatalf("importOutcomeRunningTTL (%s) must outlast the import read ceiling (%s): a key expiring mid-upload can begin again",
			importOutcomeRunningTTL, defaultImportReadCeiling)
	}
	g := newImportOutcomeRegistry()
	clock := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return clock }

	if !g.begin("u1", "k1") {
		t.Fatal("first begin refused")
	}
	if g.begin("u1", "k1") {
		t.Error("a second begin on a RUNNING key was accepted: two uploads would race to report")
	}
	if !g.begin("u2", "k1") {
		t.Error("the same key under another user was refused: keys are per user")
	}
	g.finish("u1", "k1", importStateComplete, "ws", "Ws", "dave")
	if !g.begin("u1", "k1") {
		t.Error("a finished key could not be reused")
	}
	g.finish("u1", "k1", importStateRemoved, "", "", "dave")
	if e, ok := g.get("u1", "k1"); !ok || e.State != importStateRemoved {
		t.Fatalf("get = %+v %v", e, ok)
	}

	// A RUNNING entry survives the TTL while its upload may still be reading
	// (the 1h read ceiling); a settled one does not (codex r2).
	g.begin("u3", "k-long-running")
	clock = clock.Add(importOutcomeTTL + time.Second)
	if _, ok := g.get("u1", "k1"); ok {
		t.Error("a settled entry outlived the TTL")
	}
	if e, ok := g.get("u3", "k-long-running"); !ok || e.State != importStateRunning {
		t.Fatal("a running entry expired at the settled TTL, while its upload may still be reading")
	}
	if g.begin("u3", "k-long-running") {
		t.Error("a long-running key could begin again")
	}
	clock = clock.Add(importOutcomeRunningTTL)
	if _, ok := g.get("u3", "k-long-running"); ok {
		t.Error("a running entry outlived importOutcomeRunningTTL")
	}

	// At the bound, the oldest SETTLED entry makes room; a running one never
	// does, because its key is still in use (codex r1).
	g2 := newImportOutcomeRegistry()
	g2.now = func() time.Time { return clock }
	clock = clock.Add(time.Millisecond)
	g2.begin("u", "running-oldest") // stays running, and is the oldest
	for i := 1; i < importOutcomeMax; i++ {
		clock = clock.Add(time.Millisecond)
		k := "settled-" + strconv.Itoa(i)
		g2.begin("u", k)
		g2.finish("u", k, importStateComplete, "", "", "")
	}
	clock = clock.Add(time.Millisecond)
	g2.begin("u", "newest-key")
	if len(g2.entries) != importOutcomeMax {
		t.Errorf("entries = %d, want the bound %d", len(g2.entries), importOutcomeMax)
	}
	if e, ok := g2.get("u", "running-oldest"); !ok || e.State != importStateRunning {
		t.Fatal("the oldest entry was evicted although it is still running")
	}
	if _, ok := g2.get("u", "settled-1"); ok {
		t.Error("the oldest SETTLED entry was not the one evicted")
	}
	if g2.begin("u", "running-oldest") {
		t.Error("a running key could begin again after the map filled")
	}

	// Full of RUNNING entries: the new attempt is accepted but untracked, so
	// its status reads unknown, and no running key loses its 409.
	g3 := newImportOutcomeRegistry()
	g3.now = func() time.Time { return clock }
	for i := 0; i < importOutcomeMax; i++ {
		g3.begin("u", "run-"+strconv.Itoa(i))
	}
	if !g3.begin("u", "overflow-key") {
		t.Fatal("an attempt over the bound was refused; it should run untracked")
	}
	g3.finish("u", "overflow-key", importStateComplete, "", "", "")
	if _, ok := g3.get("u", "overflow-key"); ok {
		t.Error("an untracked attempt was recorded")
	}
	if g3.begin("u", "run-0") {
		t.Error("a running key lost its 409 when the map was full")
	}
}

// A panic after mint skips every report in the handler; the attempt must
// still end, as unknown, rather than read running until the TTL (codex r1).
func TestImportBundle_BUG3475_PanicRecordsUnknown(t *testing.T) {
	srv := attachmentsServerOn(t, store.DriverSQLite)
	_, tok := memberImporter(t, srv)
	panicAfterMint(srv)
	rr := importKeyed(srv, "panicky", "key-panicked-1", bytes.NewReader(realBundleWithBlob(t)), tok)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("a panic must still answer 500, got %d: %s", rr.Code, rr.Body.String())
	}
	if code, st := importStatus(t, srv, "key-panicked-1", tok); code != http.StatusOK || st.State != importStateUnknown {
		t.Fatalf("status = %d %+v, want unknown", code, st)
	}
}
