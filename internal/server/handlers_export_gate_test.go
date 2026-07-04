package server

import (
	"net/http"
	"testing"
)

// TestExportWorkspace_RestrictedOwner_Forbidden pins BUG-1922: workspace
// export (JSON form) is an owner-only backup/portability affordance, not a
// visibility-scoped view. A workspace-role "owner" who is independently
// restricted via collection_access="specific" (member_collection_access has
// no role exclusion — BUG-1920) must be denied the export outright rather
// than receiving a filtered subset, over either auth class.
func TestExportWorkspace_RestrictedOwner_Forbidden(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	path := "/api/v1/workspaces/" + f.ws.Slug + "/export"
	rr := doRequestWithHeaders(f.srv, "GET", path, nil, f.bearerHeaders())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bearer restricted owner export (json): expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithCookie(f.srv, "GET", path, nil, f.sessionToken)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("session restricted owner export (json): expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestExportWorkspaceBundle_RestrictedOwner_Forbidden is the tar.gz-bundle
// twin of TestExportWorkspace_RestrictedOwner_Forbidden. It also pins that
// the 403 is written before any bundle bytes are streamed — a restricted
// owner must never see even a truncated tar, which would still contain
// pad-export.json with the full unfiltered workspace inside it.
func TestExportWorkspaceBundle_RestrictedOwner_Forbidden(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)

	path := "/api/v1/workspaces/" + f.ws.Slug + "/export?format=tar"
	rr := doRequestWithHeaders(f.srv, "GET", path, nil, f.bearerHeaders())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bearer restricted owner export (tar): expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct == "application/gzip" {
		t.Fatalf("bearer restricted owner export (tar): got gzip Content-Type on a 403 — bundle stream started before the gate")
	}
	body := rr.Body.Bytes()
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		t.Fatalf("bearer restricted owner export (tar): response body starts with gzip magic bytes — bundle content leaked on a 403")
	}

	rr = doRequestWithCookie(f.srv, "GET", path, nil, f.sessionToken)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("session restricted owner export (tar): expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct == "application/gzip" {
		t.Fatalf("session restricted owner export (tar): got gzip Content-Type on a 403 — bundle stream started before the gate")
	}
	body = rr.Body.Bytes()
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		t.Fatalf("session restricted owner export (tar): response body starts with gzip magic bytes — bundle content leaked on a 403")
	}
}

// TestExportWorkspace_UnrestrictedOwner_OK confirms BUG-1922's gate doesn't
// regress the common case: an owner with unrestricted (mode "all")
// workspace access still gets the JSON export exactly as before. The
// tar-bundle equivalent is already pinned by TestExportBundle_RoundTrip.
func TestExportWorkspace_UnrestrictedOwner_OK(t *testing.T) {
	srv := testServer(t)
	slug := createWSForTest(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted owner export (json): expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
