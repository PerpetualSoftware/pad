package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// The workspace import's --repair-nul flag, measured AT THE DOOR (DOC-2823 S3,
// BUG-2810).
//
// The unit-level tests in nul_repair_differential_test.go measure the repair
// and the remedy string. These drive the handler, because the defect BUG-2810
// describes is a round trip — "the server emits a payload it will then refuse"
// — and the flag's whole job is to close it. A door-level test is also what
// catches the wiring mistake this file's first version had: the JSON path
// repaired the body and then discarded the count, so the header reported 0
// while the import had rewritten a value.

// nulExportBody builds the payload an export of an AFFECTED database produces.
//
// The escape is not typed: a real NUL is put in a Go string and json.Marshal
// writes it out as the six-character escape, which is exactly how the export
// endpoint produces one. Typing the escape here would make the fixture a
// statement about this file rather than about the export.
func nulExportBody(t *testing.T, content string) []byte {
	t.Helper()
	b, err := json.Marshal(models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Affected", Slug: "affected"},
		Collections: []models.CollectionExport{{
			ID: "col-1", Name: "Tasks", Slug: "tasks", Schema: "{}", Settings: "{}",
		}},
		Items: []models.ItemExport{{
			ID: "item-1", CollectionID: "col-1", Title: "Subject", Slug: "subject",
			Content: content, Fields: "{}", Tags: "[]", ItemNumber: 1,
		}},
	})
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	return b
}

func importWithQuery(t *testing.T, srv *Server, query string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/workspaces/import"+query, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.handleImportWorkspace(rr, r)
	return rr
}

// TestImportOfAnAffectedExportIsRefusedAndTheRemedyWorks is BUG-2810's filed
// symptom and its fix, in one test, in that order.
func TestImportOfAnAffectedExportIsRefusedAndTheRemedyWorks(t *testing.T) {
	affected := nulExportBody(t, "before"+textguard.NUL+"after")

	// The fixture must actually carry the escape, or everything below passes
	// while measuring nothing.
	if !bytes.Contains(affected, []byte(textguard.EscNUL)) {
		t.Fatalf("fixture does not carry the escape: %s", affected)
	}

	t.Run("strict refuses it and names the flag", func(t *testing.T) {
		srv := testServer(t)
		rr := importWithQuery(t, srv, "", affected)

		if rr.Code != http.StatusBadRequest {
			t.Fatalf("strict import returned %d, want 400: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "--repair-nul") {
			t.Errorf("the refusal does not name the remedy: %s", body)
		}
		if !strings.Contains(body, "pad db repair-nul") {
			t.Errorf("the refusal does not name the database-side repair: %s", body)
		}
	})

	t.Run("the named flag accepts the same body and reports the count", func(t *testing.T) {
		srv := testServer(t)
		rr := importWithQuery(t, srv, "?repair_nul=true", affected)

		if rr.Code != http.StatusCreated {
			t.Fatalf("import with --repair-nul returned %d, want 201: %s", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get(NULRepairHeader); got != "1" {
			t.Errorf("%s = %q, want \"1\" — the operator is told what was rewritten, and a count that "+
				"is always 0 is how the repair silently stops being reported", NULRepairHeader, got)
		}

		// And the stored value is the repaired one, not the original and not
		// something blanked. This is the assertion that makes the 201 mean
		// something.
		var ws models.Workspace
		if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		items, err := srv.store.ListItems(ws.ID, models.ItemListParams{})
		if err != nil {
			t.Fatalf("list items: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("imported %d items, want 1", len(items))
		}
		if want := "before" + textguard.Replacement + "after"; items[0].Content != want {
			t.Errorf("imported content = %q, want %q", items[0].Content, want)
		}
	})

	t.Run("a clean export reports zero rather than nothing", func(t *testing.T) {
		srv := testServer(t)
		rr := importWithQuery(t, srv, "?repair_nul=true", nulExportBody(t, "ordinary content"))

		if rr.Code != http.StatusCreated {
			t.Fatalf("clean import with the flag returned %d, want 201: %s", rr.Code, rr.Body.String())
		}
		// Zero is a real answer: it tells the operator the export did not need
		// the flag. An absent header would be indistinguishable from an old
		// server that ignored it.
		if got := rr.Header().Get(NULRepairHeader); got != "0" {
			t.Errorf("%s = %q, want \"0\"", NULRepairHeader, got)
		}
	})

	t.Run("the flag does not repair without being asked", func(t *testing.T) {
		// The control for the whole feature: an import that repaired by
		// default would pass every assertion above and break the strict
		// posture Dave's ruling keeps.
		srv := testServer(t)
		rr := importWithQuery(t, srv, "?repair_nul=false", affected)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("repair_nul=false returned %d, want 400 — the default must stay strict: %s",
				rr.Code, rr.Body.String())
		}
	})
}

// TestImportBundle_RepairFlagCoversTheBundleDoorToo is the parity leg.
//
// handleImportWorkspaceBundle is reachable only through the Content-Type
// dispatch in handleImportWorkspace, so a flag honoured below that dispatch
// would work on the JSON path and silently do nothing on the tar.gz one — which
// is the path a real `pad workspace export --bundle` produces. BUG-2803's round
// 3 found the strict check missing on exactly this door for the same reason,
// and the plan-limit unit found its gate missing there too.
func TestImportBundle_RepairFlagCoversTheBundleDoorToo(t *testing.T) {
	src, srcSlug := testServerWithAttachments(t)
	rr := doRequest(src, "GET", "/api/v1/workspaces/"+srcSlug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export src: %d %s", rr.Code, rr.Body.String())
	}
	clean := rr.Body.String()

	// Same fixture shape as TestImportBundle_RefusesNULInExport: the escape
	// goes into the exported workspace NAME, because a bundle is bytes on the
	// wire and that is the shape an affected export carries.
	withNUL := strings.Replace(clean, `"name":"`, `"name":"a`+textguard.EscNUL+`b `, 1)
	if withNUL == clean {
		t.Fatal("fixture did not modify the export; the probe would be vacuous")
	}

	bundle := func(exportJSON string) []byte {
		var buf bytes.Buffer
		gzw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gzw)
		if err := tw.WriteHeader(&tar.Header{
			Name: "pad-export.json", Mode: 0o644, Size: int64(len(exportJSON)),
		}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(exportJSON)); err != nil {
			t.Fatalf("write export: %v", err)
		}
		tw.Close()
		gzw.Close()
		return buf.Bytes()
	}

	post := func(query string, body []byte) *httptest.ResponseRecorder {
		dest, _ := testServerWithAttachments(t)
		req := httptest.NewRequest("POST", "/api/v1/workspaces/import"+query, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/gzip")
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		dest.ServeHTTP(rec, req)
		return rec
	}

	// Strict still refuses, and names the remedy.
	strict := post("?name=Strict", bundle(withNUL))
	if strict.Code != http.StatusBadRequest {
		t.Fatalf("strict bundle import returned %d, want 400: %s", strict.Code, strict.Body.String())
	}
	if !strings.Contains(strict.Body.String(), "--repair-nul") {
		t.Errorf("the bundle refusal does not name the remedy: %s", strict.Body.String())
	}

	// And the named flag accepts the same bytes.
	repaired := post("?name=Repaired&repair_nul=true", bundle(withNUL))
	if repaired.Code != http.StatusOK && repaired.Code != http.StatusCreated {
		t.Fatalf("bundle import with --repair-nul returned %d, want 201 — the flag is honoured on the "+
			"JSON door and not this one: %s", repaired.Code, repaired.Body.String())
	}
	if got := repaired.Header().Get(NULRepairHeader); got != "1" {
		t.Errorf("%s = %q, want \"1\"", NULRepairHeader, got)
	}
}
