package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// doArtifactRequest posts a raw (non-JSON) body to an artifact-import endpoint.
func doArtifactRequest(srv *Server, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/markdown")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// doArtifactRequestWithCookie is doArtifactRequest with session + CSRF cookies,
// for the cloud-mode paths that require authentication.
func doArtifactRequestWithCookie(srv *Server, method, path string, body []byte, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/markdown")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: token})
	// Double-submit CSRF cookie/header — any fixed 64-char hex string works.
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: testCSRF})
	req.Header.Set("X-CSRF-Token", testCSRF)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// ---- Export ----

func TestExportPlaybookArtifactRoundTrip(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	pb := createItem(t, srv, ws, "playbooks", map[string]interface{}{
		"title":   "Ship It",
		"content": "## Steps\n\n1. Build\n2. Ship\n",
		"fields":  `{"status":"active","trigger":"on-release","scope":"all","invocation_slug":"ship-it"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+pb.Slug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("unexpected Content-Type %q", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, pb.Slug+".pad.md") {
		t.Errorf("unexpected Content-Disposition %q", cd)
	}

	art, err := artifact.Decode(rr.Body.Bytes())
	if err != nil {
		t.Fatalf("decode exported artifact: %v", err)
	}
	if art.Kind != artifact.KindPlaybook {
		t.Errorf("expected kind playbook, got %q", art.Kind)
	}
	if art.Title != "Ship It" {
		t.Errorf("expected title 'Ship It', got %q", art.Title)
	}
	if art.Fields["status"] != "active" {
		t.Errorf("expected status active, got %v", art.Fields["status"])
	}
	if art.Fields["invocation_slug"] != "ship-it" {
		t.Errorf("expected invocation_slug ship-it, got %v", art.Fields["invocation_slug"])
	}
	if art.Provenance.Workspace != ws {
		t.Errorf("expected provenance workspace %q, got %q", ws, art.Provenance.Workspace)
	}
	if art.Provenance.ExportedAt == "" {
		t.Error("expected non-empty exported_at")
	}
}

func TestExportConventionArtifactRoundTrip(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	cv := createItem(t, srv, ws, "conventions", map[string]interface{}{
		"title":   "Always Test",
		"content": "Run the test suite before committing.\n",
		"fields":  `{"status":"active","trigger":"on-commit","scope":"all","priority":"must"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+cv.Slug+"/export", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("export: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	art, err := artifact.Decode(rr.Body.Bytes())
	if err != nil {
		t.Fatalf("decode exported artifact: %v", err)
	}
	if art.Kind != artifact.KindConvention {
		t.Errorf("expected kind convention, got %q", art.Kind)
	}
	if art.Title != "Always Test" {
		t.Errorf("expected title 'Always Test', got %q", art.Title)
	}
	if art.Fields["priority"] != "must" {
		t.Errorf("expected priority must, got %v", art.Fields["priority"])
	}
}

func TestExportNonArtifactCollectionRejected(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	task := createItem(t, srv, ws, "tasks", map[string]interface{}{
		"title":  "A task",
		"fields": `{"status":"open"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+task.Slug+"/export", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("export task: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestExportInvisibleItemNotAuthorized(t *testing.T) {
	srv := testServer(t)
	// Activate auth so per-item visibility is enforced.
	bootstrapFirstUser(t, srv, "admin@test.com", "Admin")

	// A guest (non-member) with an item grant on an unrelated task can see
	// only that task — not a playbook in the system playbooks collection.
	// Guests, unlike restricted members, do NOT get the system-collection
	// access union, so the playbook is genuinely invisible to them.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "GuestExport"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := srv.store.SeedCollectionsFromTemplate(ws.ID, ""); err != nil {
		t.Fatalf("seed collections: %v", err)
	}
	tasks, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("get tasks collection: %v", err)
	}
	grantedTask, err := srv.store.CreateItem(ws.ID, tasks.ID, models.ItemCreate{
		Title: "Granted Task", Fields: `{}`,
	})
	if err != nil {
		t.Fatalf("CreateItem task: %v", err)
	}
	playbooks, err := srv.store.GetCollectionBySlug(ws.ID, "playbooks")
	if err != nil || playbooks == nil {
		t.Fatalf("get playbooks collection: %v", err)
	}
	pb, err := srv.store.CreateItem(ws.ID, playbooks.ID, models.ItemCreate{
		Title: "Secret", Fields: `{"status":"draft"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem playbook: %v", err)
	}

	guest, err := srv.store.CreateUser(models.UserCreate{
		Email:    "guest@test.com",
		Name:     "Guest",
		Password: "correct-horse-battery-staple",
		Role:     "member",
	})
	if err != nil {
		t.Fatalf("CreateUser guest: %v", err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, grantedTask.ID, guest.ID, "edit", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	token, err := srv.store.CreateSession(guest.ID, "go-test", "127.0.0.1", "go-test", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Target playbook → 404 (guest can't see it).
	req := httptest.NewRequest("GET",
		"/api/v1/workspaces/"+ws.Slug+"/items/"+pb.Slug+"/export", nil)
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: token})
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("export invisible item: expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---- Safety layer ----

func TestImportArtifactOversizedRejected(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)
	srv.SetImportArtifactMaxBytes(256) // tiny cap for the test

	big := "---\npad_artifact: convention\nformat_version: 1\ntitle: " +
		strings.Repeat("A", 1024) + "\n---\n\nbody\n"

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", []byte(big))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized import: expected 413, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestImportArtifactBillionLaughsRejected(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	// Classic alias-storm. Anchors/aliases are disallowed by the guard, so
	// this is rejected before artifact.Decode ever expands it.
	bomb := `---
pad_artifact: convention
format_version: 1
title: bomb
a: &a ["x","x","x","x","x","x","x","x","x"]
b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]
c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]
---

body
`
	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", []byte(bomb))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("billion-laughs: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unsafe_yaml") {
		t.Errorf("expected unsafe_yaml error code, got: %s", rr.Body.String())
	}
}

func TestImportArtifactDeepNestingRejected(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	// Build a deeply-nested flow sequence that exceeds maxFrontmatterDepth.
	var b strings.Builder
	b.WriteString("---\npad_artifact: convention\nformat_version: 1\ntitle: deep\ndeep: ")
	depth := maxFrontmatterDepth + 5
	b.WriteString(strings.Repeat("[", depth))
	b.WriteString("1")
	b.WriteString(strings.Repeat("]", depth))
	b.WriteString("\n---\n\nbody\n")

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", []byte(b.String()))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("deep-nesting: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unsafe_yaml") {
		t.Errorf("expected unsafe_yaml error code, got: %s", rr.Body.String())
	}
}

func TestImportArtifactNormalPasses(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Clean Convention",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all", "priority": "must"},
		Body:          "A clean convention.\n",
	}
	data, err := artifact.Encode(art)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr.Code != http.StatusCreated {
		t.Fatalf("normal import: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ---- Import: validation parity with the create path ----

// TestImportArtifactEmptyTitleRejected confirms an artifact with no title is
// rejected with 400, matching handleCreateItem's "Title is required" gate.
// artifact.Decode tolerates a missing/blank title (it would otherwise produce
// an "untitled" item), so the import handler must enforce it explicitly.
func TestImportArtifactEmptyTitleRejected(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	// Whitespace-only title — must be treated the same as empty.
	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "   ",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all"},
		Body:          "x\n",
	}
	data, err := artifact.Encode(art)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty-title import: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Title is required") {
		t.Errorf("expected 'Title is required' message, got: %s", rr.Body.String())
	}
}

// TestImportArtifactRejectedWhenOverWorkspaceItemQuota confirms the import path
// enforces the same workspace item-count limit handleCreateItem does, returning
// the same 403 plan_limit_exceeded response when the cap is hit.
func TestImportArtifactRejectedWhenOverWorkspaceItemQuota(t *testing.T) {
	srv := testServer(t)

	// Bootstrap the first admin (before cloud mode — bootstrap is disabled in
	// cloud mode) and create a workspace they own.
	token := bootstrapFirstUser(t, srv, "admin@test.com", "Admin")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Quota"}, token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	// Pin the workspace owner to a zero item-per-workspace limit, then turn on
	// cloud mode so enforcePlanLimit actually runs (it's a no-op self-hosted).
	owner, err := srv.store.GetUserByEmail("admin@test.com")
	if err != nil || owner == nil {
		t.Fatalf("get owner: %v", err)
	}
	if err := srv.store.SetUserPlanOverrides(owner.ID, `{"items_per_workspace":0}`); err != nil {
		t.Fatalf("set plan overrides: %v", err)
	}
	srv.SetCloudMode("cloud-secret")

	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Over Quota",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all"},
		Body:          "x\n",
	}
	data, err := artifact.Encode(art)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	rr = doArtifactRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/import-artifact", data, token)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("over-quota import: expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plan_limit_exceeded") {
		t.Errorf("expected plan_limit_exceeded error code, got: %s", rr.Body.String())
	}
}

// ---- Import ----

func TestImportArtifactForcesStatusDraft(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Was Active",
		Fields:        map[string]any{"status": "active", "trigger": "on-commit", "scope": "all", "priority": "should"},
		Body:          "x\n",
	}
	data, _ := artifact.Encode(art)

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp artifactImportResponse
	parseJSON(t, rr, &resp)
	if resp.Ref == "" {
		t.Error("expected a non-empty ref")
	}

	created := getItem(t, srv, ws, resp.Slug)
	if got := fieldString(t, created.Fields, "status"); got != "draft" {
		t.Errorf("expected status forced to draft, got %q", got)
	}
}

func TestImportArtifactBlanksForeignSelectValue(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	art := artifact.Artifact{
		Kind:          artifact.KindConvention,
		FormatVersion: artifact.FormatVersion,
		Title:         "Foreign Trigger",
		// "on-candidate-advance" is a hiring-template trigger, not in the
		// software convention vocabulary this workspace ships.
		Fields: map[string]any{"status": "active", "trigger": "on-candidate-advance", "scope": "all"},
		Body:   "x\n",
	}
	data, _ := artifact.Encode(art)

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp artifactImportResponse
	parseJSON(t, rr, &resp)
	if len(resp.Warnings) == 0 {
		t.Fatal("expected a warning about the foreign trigger value")
	}
	foundWarn := false
	for _, wmsg := range resp.Warnings {
		if strings.Contains(wmsg, "trigger") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("expected a trigger warning, got %v", resp.Warnings)
	}

	created := getItem(t, srv, ws, resp.Slug)
	if got := fieldString(t, created.Fields, "trigger"); got != "" {
		t.Errorf("expected trigger blanked, got %q", got)
	}
}

func TestImportArtifactInvocationSlugCollision(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	// Seed an existing playbook that already owns the "ship" slug.
	createItem(t, srv, ws, "playbooks", map[string]interface{}{
		"title":  "Existing Ship",
		"fields": `{"status":"active","invocation_slug":"ship"}`,
	})

	art := artifact.Artifact{
		Kind:          artifact.KindPlaybook,
		FormatVersion: artifact.FormatVersion,
		Title:         "Imported Ship",
		Fields:        map[string]any{"status": "active", "invocation_slug": "ship"},
		Body:          "x\n",
	}
	data, _ := artifact.Encode(art)

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp artifactImportResponse
	parseJSON(t, rr, &resp)

	if len(resp.Warnings) == 0 {
		t.Fatal("expected a slug-collision warning")
	}

	created := getItem(t, srv, ws, resp.Slug)
	if got := fieldString(t, created.Fields, "invocation_slug"); got != "ship-2" {
		t.Errorf("expected invocation_slug suffixed to ship-2, got %q", got)
	}
}

func TestImportArtifactCreateSideEffectsFire(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	art := artifact.Artifact{
		Kind:          artifact.KindPlaybook,
		FormatVersion: artifact.FormatVersion,
		Title:         "With Activity",
		Fields:        map[string]any{"status": "active", "invocation_slug": "with-activity"},
		Body:          "x\n",
	}
	data, _ := artifact.Encode(art)

	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", data)
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp artifactImportResponse
	parseJSON(t, rr, &resp)

	// The created item must have a "created" activity entry, exactly like
	// any other create path — confirming createItemChecked's side effects
	// (activity log) ran for the import.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+resp.Slug+"/activity", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("activity: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var activities []models.Activity
	parseJSON(t, rr, &activities)
	foundCreated := false
	for _, a := range activities {
		if a.Action == "created" {
			foundCreated = true
		}
	}
	if !foundCreated {
		t.Errorf("expected a 'created' activity entry, got %+v", activities)
	}
}

// ---- helpers ----

func getItem(t *testing.T, srv *Server, ws, slug string) models.Item {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get item %s: expected 200, got %d: %s", slug, rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	return item
}

// fieldString parses an item's fields JSON and returns the string value at
// key (or "" if absent).
func fieldString(t *testing.T, fieldsJSON, key string) string {
	t.Helper()
	if fieldsJSON == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &m); err != nil {
		t.Fatalf("parse fields JSON: %v", err)
	}
	s, _ := m[key].(string)
	return s
}
