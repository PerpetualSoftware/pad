package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-2793. `POST /workspaces` enforces the user-scoped `workspaces` plan
// limit; `POST /workspaces/import` did not, and it mints a workspace through
// the same store.CreateWorkspace. A user at their plan's limit could exceed it
// by exporting any workspace and importing it back.
//
// This is the SECOND gate on workspace creation the import door skipped — the
// first was the OAuth consent gate (IDEA-2756, PR #1212). The tests below are
// written against the door rather than against the limiter, because the
// limiter was never broken: what was missing was the call, and where it sits.

// importLimitFixture builds a cloud-mode server and a user who owns exactly
// `owned` workspaces.
func importLimitFixture(t *testing.T, owned int) (*Server, *models.User) {
	t.Helper()
	srv := testServer(t)
	srv.cloudMode = true

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner",
		Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	for i := 0; i < owned; i++ {
		if _, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
			Name: "Owned " + string(rune('A'+i)), OwnerID: user.ID,
		}); err != nil {
			t.Fatalf("create workspace %d: %v", i, err)
		}
	}
	return srv, user
}

// importRequest drives handleImportWorkspace directly with the caller
// attached, which is how the router presents an authenticated request.
// contentType selects the JSON path or the tar.gz bundle path.
func importRequest(t *testing.T, srv *Server, user *models.User, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/workspaces/import", bytes.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	r.RemoteAddr = "192.0.2.1:1234"
	if user != nil {
		r = r.WithContext(WithCurrentUser(r.Context(), user))
	}
	rr := httptest.NewRecorder()
	srv.handleImportWorkspace(rr, r)
	return rr
}

func exportBody(t *testing.T) []byte {
	t.Helper()
	// Version 1 is REQUIRED. Without it the import fails with a 500 before it
	// reaches anything this file is about — and the success controls below,
	// which assert only "not 403", passed on that 500 (codex round 1). A
	// control that cannot tell success from a server error controls nothing.
	b, err := json.Marshal(models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Imported", Slug: "imported"},
	})
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	return b
}

// TestImportWorkspace_EnforcesThePlanLimit is the defect itself, on the JSON
// path. The free plan allows store.DefaultFreeLimits.Workspaces; a user who
// already owns that many must be refused.
func TestImportWorkspace_EnforcesThePlanLimit(t *testing.T) {
	atLimit := store.DefaultFreeLimits.Workspaces
	srv, user := importLimitFixture(t, atLimit)

	rr := importRequest(t, srv, user, "application/json", exportBody(t))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("import at the plan limit returned %d, want 403: %s", rr.Code, rr.Body.String())
	}
	if b := rr.Body.String(); !strings.Contains(b, "plan_limit_exceeded") {
		t.Errorf("response lacks the code a client switches on: %s", b)
	}
}

// TestImportWorkspace_EnforcesThePlanLimitOnTheBundlePathToo is the reason the
// gate's PLACEMENT is the load-bearing part rather than the call.
//
// handleImportWorkspaceBundle is reachable only through this handler's
// Content-Type dispatch. A gate added below that dispatch would cover the JSON
// path and leave the tar.gz path — the one that carries attachments, and the
// one a real export produces — wide open, while the test above stayed green.
//
// The body is deliberately not a valid bundle: the refusal must happen before
// anything reads it, so an invalid body reaching a 403 rather than a parse
// error is itself the assertion.
func TestImportWorkspace_EnforcesThePlanLimitOnTheBundlePathToo(t *testing.T) {
	atLimit := store.DefaultFreeLimits.Workspaces
	srv, user := importLimitFixture(t, atLimit)

	rr := importRequest(t, srv, user, "application/gzip", []byte("not a real gzip bundle"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("bundle-path import at the plan limit returned %d, want 403 — a gate below the "+
			"Content-Type dispatch would leave this path open: %s", rr.Code, rr.Body.String())
	}
	if b := rr.Body.String(); !strings.Contains(b, "plan_limit_exceeded") {
		t.Errorf("response lacks the plan-limit code: %s", b)
	}
}

// TestImportWorkspace_EnforcesThePlanLimitBeforeReadingTheJSONBody is the
// JSON-path half of the placement claim, and the refusal test above cannot
// make it: that one sends VALID JSON, so a gate placed after the decode would
// still return 403 and it would stay green (codex round 3).
//
// The code comment claims the gate sits above EITHER body read. The bundle
// test proves it for gzip. This proves it for JSON, by sending a body that
// cannot be decoded: reaching 403 rather than a decode error is only possible
// if nothing read the body first.
func TestImportWorkspace_EnforcesThePlanLimitBeforeReadingTheJSONBody(t *testing.T) {
	atLimit := store.DefaultFreeLimits.Workspaces
	srv, user := importLimitFixture(t, atLimit)

	rr := importRequest(t, srv, user, "application/json", []byte("{this is not json"))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("at-limit import with an undecodable body returned %d, want 403 — the gate is "+
			"running after the JSON decode: %s", rr.Code, rr.Body.String())
	}
	if b := rr.Body.String(); !strings.Contains(b, "plan_limit_exceeded") {
		t.Errorf("refused for the wrong reason — want the plan-limit code, got: %s", b)
	}
}

// TestImportWorkspace_UnderTheLimitIsNotRefused is the control. Without it, a
// gate that refused every import — or one wired to the wrong feature key —
// passes both tests above while breaking the feature outright.
func TestImportWorkspace_UnderTheLimitIsNotRefused(t *testing.T) {
	srv, user := importLimitFixture(t, store.DefaultFreeLimits.Workspaces-1)

	rr := importRequest(t, srv, user, "application/json", exportBody(t))

	if rr.Code != http.StatusCreated {
		t.Fatalf("import UNDER the plan limit returned %d, want 201 — asserting merely "+
			"\"not 403\" would pass on a 500 and prove nothing: %s", rr.Code, rr.Body.String())
	}
}

// TestImportWorkspace_SelfHostedIsUnaffected pins the other half of the
// obligation: enforceUserPlanLimit is a no-op off cloud, and this fix must not
// quietly introduce a limit on self-hosted instances, which have no plans.
func TestImportWorkspace_SelfHostedIsUnaffected(t *testing.T) {
	atLimit := store.DefaultFreeLimits.Workspaces
	srv, user := importLimitFixture(t, atLimit)
	srv.cloudMode = false // the only difference from the refusing case

	rr := importRequest(t, srv, user, "application/json", exportBody(t))

	if rr.Code != http.StatusCreated {
		t.Fatalf("self-hosted import returned %d, want 201 — a plan limit must not apply where "+
			"there are no plans: %s", rr.Code, rr.Body.String())
	}
}

// TestImportWorkspace_NoResolvedUserIsNotCharged mirrors the create side's
// `userID != ""` guard. A legacy workspace token resolves no user, and there
// is nobody to charge — the guard is not defensive padding, it is the
// difference between "no limit applies" and a nil lookup.
//
// SCOPE, stated because this test locks in a 201 and someone will read that as
// approval: it pins the GUARD's behaviour, not a judgement that userless
// workspace creation is fine. It also drives the handler directly rather than
// through a real legacy token, so it does not prove that token shape reaches
// here — only that the guard does what create's does when no user resolves.
// Whether these doors should mint unowned workspaces at all is BUG-2809.
func TestImportWorkspace_NoResolvedUserIsNotCharged(t *testing.T) {
	srv, user := importLimitFixture(t, store.DefaultFreeLimits.Workspaces)

	before, err := srv.store.CheckUserLimit(user.ID, "workspaces")
	if err != nil {
		t.Fatalf("read the user's limit: %v", err)
	}

	rr := importRequest(t, srv, nil, "application/json", exportBody(t))

	if rr.Code != http.StatusCreated {
		t.Fatalf("import with no resolved user returned %d, want 201 — there is nobody to charge: %s",
			rr.Code, rr.Body.String())
	}

	// "Not charged" asserted as a FACT about the data, not inferred from a
	// status code (codex round 3). A regression that quietly attributed the
	// import to the at-limit fixture user would return 201 too, and pass on
	// the check above alone.
	after, err := srv.store.CheckUserLimit(user.ID, "workspaces")
	if err != nil {
		t.Fatalf("re-check the user's limit: %v", err)
	}
	if after.Current != before.Current {
		t.Errorf("the fixture user's workspace count moved %d -> %d; an import with no resolved "+
			"user was attributed to them", before.Current, after.Current)
	}

	var created models.Workspace
	parseJSON(t, rr, &created)
	if created.OwnerID != "" {
		t.Errorf("workspace created with owner %q by a caller with no resolved user", created.OwnerID)
	}
}
