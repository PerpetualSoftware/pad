package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/xarmian/pad/internal/models"
)

// testStore creates a Store for testing. When PAD_TEST_POSTGRES_URL is set,
// it creates an isolated PostgreSQL database; otherwise it falls back to a
// temporary SQLite file. Every test gets its own database so tests never
// interfere with each other.
func testStore(t *testing.T) *Store {
	t.Helper()

	if pgURL := os.Getenv("PAD_TEST_POSTGRES_URL"); pgURL != "" {
		return testStorePostgres(t, pgURL)
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// testStorePostgres creates an isolated test database on the PostgreSQL server.
// It connects to the base URL, creates a randomly-named database, runs
// migrations, and drops the database when the test finishes.
func testStorePostgres(t *testing.T, baseURL string) *Store {
	t.Helper()

	// Generate a unique database name for this test.
	dbName := "pad_test_" + uuid.New().String()[:8]

	// Connect to the default "pad" database to issue CREATE/DROP DATABASE.
	adminStore, err := newPostgresConn(baseURL)
	if err != nil {
		t.Fatalf("connect to postgres for admin: %v", err)
	}

	// CREATE DATABASE cannot run inside a transaction.
	if _, err := adminStore.db.Exec(fmt.Sprintf("CREATE DATABASE %s", dbName)); err != nil {
		adminStore.Close()
		t.Fatalf("create test database %s: %v", dbName, err)
	}
	adminStore.Close()

	// Build the connection string for the new database.
	testURL := replaceDBName(baseURL, dbName)

	s, err := NewPostgres(testURL)
	if err != nil {
		// Clean up the database we just created.
		if admin2, err2 := newPostgresConn(baseURL); err2 == nil {
			admin2.db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", dbName))
			admin2.Close()
		}
		t.Fatalf("open test postgres store: %v", err)
	}

	t.Cleanup(func() {
		s.Close()
		// Drop the test database.
		if admin, err := newPostgresConn(baseURL); err == nil {
			admin.db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
			admin.Close()
		}
	})

	return s
}

// newPostgresConn opens a raw postgres connection (no migrations).
func newPostgresConn(connStr string) (*Store, error) {
	db, err := openPostgresDB(connStr)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, dialect: &postgresDialect{}}, nil
}

// replaceDBName swaps the database name in a postgres:// URL.
// e.g. "postgres://pad:pad@localhost:5432/pad?sslmode=disable"
// becomes "postgres://pad:pad@localhost:5432/newdb?sslmode=disable"
func replaceDBName(connStr, newDB string) string {
	// Find the last '/' before any '?' query string.
	query := ""
	base := connStr
	if idx := len(connStr) - len(connStr); idx >= 0 {
		if qIdx := indexOf(connStr, '?'); qIdx >= 0 {
			query = connStr[qIdx:]
			base = connStr[:qIdx]
		}
	}
	// Find the last '/' in the base to replace the db name.
	if lastSlash := lastIndexOf(base, '/'); lastSlash >= 0 {
		return base[:lastSlash+1] + newDB + query
	}
	return connStr
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func lastIndexOf(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func createTestWorkspace(t *testing.T, s *Store, name string) *models.Workspace {
	t.Helper()
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name})
	if err != nil {
		t.Fatalf("failed to create workspace: %v", err)
	}
	return ws
}

func createTestDoc(t *testing.T, s *Store, workspaceID, title, content string) *models.Document {
	t.Helper()
	doc, err := s.CreateDocument(workspaceID, models.DocumentCreate{
		Title:   title,
		Content: content,
		DocType: "notes",
		Status:  "active",
	})
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	return doc
}

func schemaFieldKeys(t *testing.T, schemaJSON string) []string {
	t.Helper()

	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		t.Fatalf("unmarshal collection schema: %v", err)
	}

	keys := make([]string, 0, len(schema.Fields))
	for _, field := range schema.Fields {
		keys = append(keys, field.Key)
	}
	return keys
}

// TestSQLiteConcurrentWritersNoBusy guards against the BUG-748-followup
// regression where Go's default deferred-mode transactions returned
// SQLITE_BUSY immediately under contention because lock-upgrade does not
// honor busy_timeout. With `_txlock=immediate` set in the DSN, every
// `db.Begin()` issues `BEGIN IMMEDIATE`, so the write lock is acquired
// up-front and concurrent writers wait via busy_timeout instead of
// failing fast.
//
// The benchmark in `bench_concurrent_test.go` quantifies throughput;
// this test specifically asserts ZERO errors under a concurrent-update
// workload that previously produced multiple `SQLITE_BUSY` returns.
func TestSQLiteConcurrentWritersNoBusy(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific concurrency test; postgres has its own MVCC semantics")
	}

	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "concurrency"})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`,
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}

	// 20 goroutines each create one item — these all open transactions
	// (tryCreateItem in items.go uses db.Begin), and previously raced
	// for the write-lock on upgrade.
	const workers = 20
	errCh := make(chan error, workers)
	doneCh := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		idx := i
		go func() {
			_, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
				Title:  fmt.Sprintf("concurrent-%d", idx),
				Fields: `{"status":"open"}`,
			})
			if err != nil {
				errCh <- err
			}
			doneCh <- struct{}{}
		}()
	}
	for i := 0; i < workers; i++ {
		<-doneCh
	}
	close(errCh)

	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}
	if len(errs) > 0 {
		t.Errorf("expected zero errors under concurrent writes, got %d:", len(errs))
		for _, e := range errs {
			t.Errorf("  - %v", e)
		}
	}
}

func TestNewStore(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("skipping SQLite-specific test when running against PostgreSQL")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer s.Close()

	// DB file should exist
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("database file was not created")
	}
}

func TestNewStorePostgres(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set, skipping PostgreSQL test")
	}

	s := testStorePostgres(t, pgURL)

	// Verify we can ping and the dialect is correct.
	if err := s.Ping(); err != nil {
		t.Fatalf("Ping() error: %v", err)
	}
	if s.dialect.Driver() != DriverPostgres {
		t.Errorf("expected DriverPostgres, got %v", s.dialect.Driver())
	}
}

// --- Workspace Tests ---

func TestWorkspaceCRUD(t *testing.T) {
	s := testStore(t)

	// Create
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "My App"})
	if err != nil {
		t.Fatalf("CreateWorkspace error: %v", err)
	}
	if ws.Name != "My App" {
		t.Errorf("expected name 'My App', got %q", ws.Name)
	}
	if ws.Slug != "my-app" {
		t.Errorf("expected slug 'my-app', got %q", ws.Slug)
	}

	// List
	list, err := s.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces error: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 workspace, got %d", len(list))
	}

	// Get by slug
	got, err := s.GetWorkspaceBySlug("my-app")
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug error: %v", err)
	}
	if got == nil || got.ID != ws.ID {
		t.Error("GetWorkspaceBySlug returned wrong workspace")
	}

	// Update
	newName := "My Updated App"
	updated, err := s.UpdateWorkspace("my-app", models.WorkspaceUpdate{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateWorkspace error: %v", err)
	}
	if updated.Name != "My Updated App" {
		t.Errorf("expected updated name, got %q", updated.Name)
	}

	// Delete (soft)
	err = s.DeleteWorkspace("my-app")
	if err != nil {
		t.Fatalf("DeleteWorkspace error: %v", err)
	}

	// Should not appear in list
	list, _ = s.ListWorkspaces()
	if len(list) != 0 {
		t.Error("deleted workspace still appears in list")
	}

	// Should not be found by slug
	got, _ = s.GetWorkspaceBySlug("my-app")
	if got != nil {
		t.Error("deleted workspace still found by slug")
	}
}

func TestWorkspaceUniqueSlug(t *testing.T) {
	s := testStore(t)

	ws1, _ := s.CreateWorkspace(models.WorkspaceCreate{Name: "Test"})
	ws2, _ := s.CreateWorkspace(models.WorkspaceCreate{Name: "Test"})

	if ws1.Slug == ws2.Slug {
		t.Error("duplicate slugs should not be allowed")
	}
	if ws2.Slug != "test-2" {
		t.Errorf("expected slug 'test-2', got %q", ws2.Slug)
	}
}

func TestWorkspaceSettingsHydrateStructuredContext(t *testing.T) {
	s := testStore(t)

	settings, err := models.SerializeWorkspaceSettings(&models.WorkspaceSettings{
		Context: &models.WorkspaceContext{
			Repositories: []models.WorkspaceRepository{
				{Name: "docapp", Role: "primary", Path: ".", Repo: "xarmian/pad"},
				{Name: "pad-web", Role: "docs", Path: "../pad-web", Repo: "xarmian/pad-web"},
			},
			Commands: &models.WorkspaceCommands{
				Build: "make install",
				Test:  "go test ./...",
				Web:   "cd web && npm run build",
			},
			Deployment: &models.WorkspaceDeployment{
				Mode:    "local",
				BaseURL: "http://127.0.0.1:7777",
			},
		},
	})
	if err != nil {
		t.Fatalf("SerializeWorkspaceSettings error: %v", err)
	}

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{
		Name:     "Machine Readable",
		Settings: settings,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace error: %v", err)
	}
	if ws.Context == nil {
		t.Fatal("expected created workspace to expose structured context")
	}
	if len(ws.Context.Repositories) != 2 {
		t.Fatalf("expected 2 repositories in context, got %#v", ws.Context.Repositories)
	}

	got, err := s.GetWorkspaceBySlug(ws.Slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug error: %v", err)
	}
	if got.Context == nil || got.Context.Commands == nil {
		t.Fatalf("expected hydrated commands, got %#v", got.Context)
	}
	if got.Context.Commands.Build != "make install" {
		t.Fatalf("expected build command make install, got %q", got.Context.Commands.Build)
	}
	if got.Context.Deployment == nil || got.Context.Deployment.BaseURL != "http://127.0.0.1:7777" {
		t.Fatalf("expected deployment base URL to round-trip, got %#v", got.Context.Deployment)
	}
}

// --- Document Tests ---

func TestDocumentCRUD(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	// Create
	doc, err := s.CreateDocument(ws.ID, models.DocumentCreate{
		Title:   "My Doc",
		Content: "Hello world",
		DocType: "notes",
		Status:  "draft",
	})
	if err != nil {
		t.Fatalf("CreateDocument error: %v", err)
	}
	if doc.Title != "My Doc" {
		t.Errorf("expected title 'My Doc', got %q", doc.Title)
	}
	if doc.Slug != "my-doc" {
		t.Errorf("expected slug 'my-doc', got %q", doc.Slug)
	}

	// Get
	got, err := s.GetDocument(doc.ID)
	if err != nil {
		t.Fatalf("GetDocument error: %v", err)
	}
	if got.Content != "Hello world" {
		t.Errorf("expected content 'Hello world', got %q", got.Content)
	}

	// Update
	newContent := "Updated content"
	updated, err := s.UpdateDocument(doc.ID, models.DocumentUpdate{
		Content: &newContent,
	})
	if err != nil {
		t.Fatalf("UpdateDocument error: %v", err)
	}
	if updated.Content != "Updated content" {
		t.Errorf("expected updated content, got %q", updated.Content)
	}

	// List
	docs, err := s.ListDocuments(ws.ID, models.DocumentListParams{})
	if err != nil {
		t.Fatalf("ListDocuments error: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("expected 1 document, got %d", len(docs))
	}

	// Delete
	err = s.DeleteDocument(doc.ID)
	if err != nil {
		t.Fatalf("DeleteDocument error: %v", err)
	}

	// Should not appear in list
	docs, _ = s.ListDocuments(ws.ID, models.DocumentListParams{})
	if len(docs) != 0 {
		t.Error("deleted document still appears in list")
	}

	// Restore
	restored, err := s.RestoreDocument(doc.ID)
	if err != nil {
		t.Fatalf("RestoreDocument error: %v", err)
	}
	if restored.Status != "draft" {
		t.Errorf("expected restored status 'draft', got %q", restored.Status)
	}

	docs, _ = s.ListDocuments(ws.ID, models.DocumentListParams{})
	if len(docs) != 1 {
		t.Error("restored document not in list")
	}
}

func TestDocumentListFilters(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	// Create docs of different types and statuses
	s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Roadmap", DocType: "roadmap", Status: "active"})
	s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Plan", DocType: "plan", Status: "active"})
	s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Notes", DocType: "notes", Status: "draft"})

	// Filter by type
	docs, _ := s.ListDocuments(ws.ID, models.DocumentListParams{Type: "roadmap"})
	if len(docs) != 1 {
		t.Errorf("type filter: expected 1, got %d", len(docs))
	}

	// Filter by status
	docs, _ = s.ListDocuments(ws.ID, models.DocumentListParams{Status: "active"})
	if len(docs) != 2 {
		t.Errorf("status filter: expected 2, got %d", len(docs))
	}

	// Filter by pinned
	pinned := true
	docs, _ = s.ListDocuments(ws.ID, models.DocumentListParams{Pinned: &pinned})
	if len(docs) != 0 {
		t.Errorf("pinned filter: expected 0, got %d", len(docs))
	}
}

func TestVersionCreation(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	doc := createTestDoc(t, s, ws.ID, "My Doc", "Version 1")

	// First content update — should create a version (no previous versions exist)
	v2 := "Version 2"
	s.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &v2})

	versions, err := s.ListVersions(doc.ID)
	if err != nil {
		t.Fatalf("ListVersions error: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("expected 1 version after first update, got %d", len(versions))
	}

	// Second rapid update (same actor, same source, within throttle) — should NOT create a version
	v3 := "Version 3"
	s.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &v3})

	versions, _ = s.ListVersions(doc.ID)
	if len(versions) != 1 {
		t.Fatalf("expected 1 version after rapid second update (throttled), got %d", len(versions))
	}

	// Resolve the version to verify content
	doc2, _ := s.GetDocument(doc.ID)
	resolved, _ := s.ListVersionsResolved(doc.ID, doc2.Content)
	if resolved[0].Content != "Version 1" {
		t.Errorf("expected resolved version content 'Version 1', got %q", resolved[0].Content)
	}
}

func TestVersionCreationActorChange(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	doc := createTestDoc(t, s, ws.ID, "My Doc", "Original")

	// Update as user
	v2 := "User edit"
	s.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &v2, LastModifiedBy: "user", Source: "web"})

	// Update as agent — should force a new version despite throttle
	v3 := "Agent edit"
	s.UpdateDocument(doc.ID, models.DocumentUpdate{Content: &v3, LastModifiedBy: "agent", Source: "cli"})

	versions, _ := s.ListVersions(doc.ID)
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions (actor change forces version), got %d", len(versions))
	}
}

func TestVersionNotCreatedWithoutContentChange(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	doc := createTestDoc(t, s, ws.ID, "My Doc", "Content")

	// Update only status — should NOT create a version
	newStatus := "active"
	s.UpdateDocument(doc.ID, models.DocumentUpdate{Status: &newStatus})

	versions, _ := s.ListVersions(doc.ID)
	if len(versions) != 0 {
		t.Errorf("expected 0 versions for non-content change, got %d", len(versions))
	}
}

func TestQuickSave(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	// First save — creates new doc
	doc, err := s.QuickSave(ws.ID, models.QuickSave{
		Title:   "Quick Doc",
		Content: "Initial",
		DocType: "notes",
	})
	if err != nil {
		t.Fatalf("QuickSave create error: %v", err)
	}
	if doc.Content != "Initial" {
		t.Errorf("expected 'Initial', got %q", doc.Content)
	}

	// Second save — updates existing doc
	doc2, err := s.QuickSave(ws.ID, models.QuickSave{
		Title:   "Quick Doc",
		Content: "Updated",
	})
	if err != nil {
		t.Fatalf("QuickSave update error: %v", err)
	}
	if doc2.ID != doc.ID {
		t.Error("quick save should have updated existing doc, not created new one")
	}
	if doc2.Content != "Updated" {
		t.Errorf("expected 'Updated', got %q", doc2.Content)
	}

	// Should also have created a version
	versions, _ := s.ListVersions(doc.ID)
	if len(versions) != 1 {
		t.Errorf("expected 1 version after quick save update, got %d", len(versions))
	}
}

func TestBulkRead(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	doc1 := createTestDoc(t, s, ws.ID, "Doc 1", "Content 1")
	doc2 := createTestDoc(t, s, ws.ID, "Doc 2", "Content 2")
	createTestDoc(t, s, ws.ID, "Doc 3", "Content 3")

	docs, err := s.BulkRead([]string{doc1.ID, doc2.ID})
	if err != nil {
		t.Fatalf("BulkRead error: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("expected 2 docs, got %d", len(docs))
	}
}

func TestDocumentLinking(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	doc1 := createTestDoc(t, s, ws.ID, "Architecture", "The architecture doc")
	createTestDoc(t, s, ws.ID, "Phase Plan", "See [[Architecture]] for details")

	// Get backlinks for Architecture
	backlinks, err := s.GetBacklinks(ws.ID, doc1.Title)
	if err != nil {
		t.Fatalf("GetBacklinks error: %v", err)
	}
	if len(backlinks) != 1 {
		t.Errorf("expected 1 backlink, got %d", len(backlinks))
	}
	if len(backlinks) > 0 && backlinks[0].Title != "Phase Plan" {
		t.Errorf("expected backlink from 'Phase Plan', got %q", backlinks[0].Title)
	}

	// Get links from Phase Plan
	phasePlan, _ := s.GetDocumentByTitle(ws.ID, "Phase Plan")
	linkedDocs, err := s.GetLinks(ws.ID, phasePlan.Content)
	if err != nil {
		t.Fatalf("GetLinks error: %v", err)
	}
	if len(linkedDocs) != 1 {
		t.Errorf("expected 1 link, got %d", len(linkedDocs))
	}
}

func TestDocumentLinkRename(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	createTestDoc(t, s, ws.ID, "Old Name", "The original doc")
	refDoc := createTestDoc(t, s, ws.ID, "Referencing Doc", "See [[Old Name]] for details")

	// Rename the document
	newTitle := "New Name"
	s.UpdateDocument(refDoc.ID, models.DocumentUpdate{}) // ensure the first doc exists

	// Get the original doc and rename it
	origDoc, _ := s.GetDocumentByTitle(ws.ID, "Old Name")
	_, err := s.UpdateDocument(origDoc.ID, models.DocumentUpdate{Title: &newTitle})
	if err != nil {
		t.Fatalf("rename error: %v", err)
	}

	// The referencing doc should now have [[New Name]]
	updated, _ := s.GetDocument(refDoc.ID)
	if updated.Content != "See [[New Name]] for details" {
		t.Errorf("link not updated: %q", updated.Content)
	}
}

func TestFTSSearch(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	s.SeedDefaultCollections(ws.ID)
	colls, _ := s.ListCollections(ws.ID)
	var docsCollID string
	for _, c := range colls {
		if c.Slug == "docs" {
			docsCollID = c.ID
			break
		}
	}

	s.CreateItem(ws.ID, docsCollID, models.ItemCreate{Title: "Auth Flow", Content: "OAuth2 authentication flow for API"})
	s.CreateItem(ws.ID, docsCollID, models.ItemCreate{Title: "Data Model", Content: "Database schema and models"})

	resp, err := s.Search(SearchParams{Query: "authentication"})
	if err != nil {
		t.Fatalf("Search error: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Errorf("expected 1 result, got %d", len(resp.Results))
	}
	if len(resp.Results) > 0 && resp.Results[0].Item.Title != "Auth Flow" {
		t.Errorf("expected 'Auth Flow', got %q", resp.Results[0].Item.Title)
	}
}

func TestFTSSearchScoped(t *testing.T) {
	s := testStore(t)
	ws1 := createTestWorkspace(t, s, "Workspace 1")
	ws2 := createTestWorkspace(t, s, "Workspace 2")
	s.SeedDefaultCollections(ws1.ID)
	s.SeedDefaultCollections(ws2.ID)

	colls1, _ := s.ListCollections(ws1.ID)
	colls2, _ := s.ListCollections(ws2.ID)
	var docs1ID, docs2ID string
	for _, c := range colls1 {
		if c.Slug == "docs" {
			docs1ID = c.ID
			break
		}
	}
	for _, c := range colls2 {
		if c.Slug == "docs" {
			docs2ID = c.ID
			break
		}
	}

	s.CreateItem(ws1.ID, docs1ID, models.ItemCreate{Title: "Doc A", Content: "authentication in workspace 1"})
	s.CreateItem(ws2.ID, docs2ID, models.ItemCreate{Title: "Doc B", Content: "authentication in workspace 2"})

	// Unscoped — should find both
	resp, _ := s.Search(SearchParams{Query: "authentication"})
	if len(resp.Results) != 2 {
		t.Errorf("unscoped: expected 2 results, got %d", len(resp.Results))
	}

	// Scoped — should find one
	resp, _ = s.Search(SearchParams{Query: "authentication", Workspace: ws1.Slug})
	if len(resp.Results) != 1 {
		t.Errorf("scoped: expected 1 result, got %d", len(resp.Results))
	}
}

func TestSearchCollectionFilter(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	s.SeedDefaultCollections(ws.ID)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID, ideasID string
	for _, c := range colls {
		if c.Slug == "tasks" {
			tasksID = c.ID
		}
		if c.Slug == "ideas" {
			ideasID = c.ID
		}
	}

	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Fix authentication bug", Fields: `{"status":"open","priority":"high"}`})
	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Refactor authentication module", Fields: `{"status":"done","priority":"medium"}`})
	s.CreateItem(ws.ID, ideasID, models.ItemCreate{Title: "New authentication provider", Fields: `{"status":"new"}`})

	// Unfiltered — should find all 3
	resp, err := s.Search(SearchParams{Query: "authentication", Workspace: ws.Slug})
	if err != nil {
		t.Fatalf("unfiltered search: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Errorf("unfiltered: expected 3 results, got %d", len(resp.Results))
	}

	// Filter by collection slug — only tasks
	resp, err = s.Search(SearchParams{Query: "authentication", Workspace: ws.Slug, Collection: "tasks"})
	if err != nil {
		t.Fatalf("collection filter search: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Errorf("collection=tasks: expected 2 results, got %d", len(resp.Results))
	}
	for _, r := range resp.Results {
		if r.Item.CollectionSlug != "tasks" {
			t.Errorf("expected collection 'tasks', got %q", r.Item.CollectionSlug)
		}
	}

	// Filter by collection slug — only ideas
	resp, err = s.Search(SearchParams{Query: "authentication", Workspace: ws.Slug, Collection: "ideas"})
	if err != nil {
		t.Fatalf("collection=ideas search: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Errorf("collection=ideas: expected 1 result, got %d", len(resp.Results))
	}
}

func TestSearchFieldFilters(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	s.SeedDefaultCollections(ws.ID)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID string
	for _, c := range colls {
		if c.Slug == "tasks" {
			tasksID = c.ID
			break
		}
	}

	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Fix login bug", Fields: `{"status":"open","priority":"high"}`})
	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Fix logout bug", Fields: `{"status":"open","priority":"low"}`})
	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Fix signup bug", Fields: `{"status":"done","priority":"high"}`})

	// Filter by status=open — should find 2
	resp, err := s.Search(SearchParams{
		Query:        "bug",
		Workspace:    ws.Slug,
		FieldFilters: map[string]string{"status": "open"},
	})
	if err != nil {
		t.Fatalf("field filter search: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Errorf("status=open: expected 2 results, got %d", len(resp.Results))
	}

	// Filter by priority=high — should find 2
	resp, err = s.Search(SearchParams{
		Query:        "bug",
		Workspace:    ws.Slug,
		FieldFilters: map[string]string{"priority": "high"},
	})
	if err != nil {
		t.Fatalf("priority filter search: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Errorf("priority=high: expected 2 results, got %d", len(resp.Results))
	}

	// Combine filters: status=open AND priority=high — should find 1
	resp, err = s.Search(SearchParams{
		Query:        "bug",
		Workspace:    ws.Slug,
		FieldFilters: map[string]string{"status": "open", "priority": "high"},
	})
	if err != nil {
		t.Fatalf("combined filter search: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Errorf("status=open+priority=high: expected 1 result, got %d", len(resp.Results))
	}
	if len(resp.Results) > 0 && resp.Results[0].Item.Title != "Fix login bug" {
		t.Errorf("expected 'Fix login bug', got %q", resp.Results[0].Item.Title)
	}
}

func TestSearchCollectionAndFieldFilters(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	s.SeedDefaultCollections(ws.ID)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID, ideasID string
	for _, c := range colls {
		if c.Slug == "tasks" {
			tasksID = c.ID
		}
		if c.Slug == "ideas" {
			ideasID = c.ID
		}
	}

	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Improve search speed", Fields: `{"status":"open","priority":"high"}`})
	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Search UI redesign", Fields: `{"status":"done","priority":"medium"}`})
	s.CreateItem(ws.ID, ideasID, models.ItemCreate{Title: "Search autocomplete feature", Fields: `{"status":"new"}`})

	// Collection + field filter: tasks with status=open
	resp, err := s.Search(SearchParams{
		Query:        "search",
		Workspace:    ws.Slug,
		Collection:   "tasks",
		FieldFilters: map[string]string{"status": "open"},
	})
	if err != nil {
		t.Fatalf("combined collection+field filter: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Errorf("tasks+status=open: expected 1 result, got %d", len(resp.Results))
	}
	if len(resp.Results) > 0 && resp.Results[0].Item.Title != "Improve search speed" {
		t.Errorf("expected 'Improve search speed', got %q", resp.Results[0].Item.Title)
	}
}

func TestSearchPagination(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	s.SeedDefaultCollections(ws.ID)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID string
	for _, c := range colls {
		if c.Slug == "tasks" {
			tasksID = c.ID
			break
		}
	}

	// Create 5 items all matching "widget"
	for i := 0; i < 5; i++ {
		s.CreateItem(ws.ID, tasksID, models.ItemCreate{
			Title:  fmt.Sprintf("Widget task %d", i+1),
			Fields: `{"status":"open","priority":"medium"}`,
		})
	}

	// Default pagination — should get all 5
	resp, err := s.Search(SearchParams{Query: "widget", Workspace: ws.Slug})
	if err != nil {
		t.Fatalf("default pagination: %v", err)
	}
	if resp.Total != 5 {
		t.Errorf("expected total=5, got %d", resp.Total)
	}
	if len(resp.Results) != 5 {
		t.Errorf("expected 5 results, got %d", len(resp.Results))
	}
	if resp.Limit != 50 {
		t.Errorf("expected default limit=50, got %d", resp.Limit)
	}

	// Limit=2 — should get 2 results but total still 5
	resp, err = s.Search(SearchParams{Query: "widget", Workspace: ws.Slug, Limit: 2})
	if err != nil {
		t.Fatalf("limit=2: %v", err)
	}
	if resp.Total != 5 {
		t.Errorf("expected total=5, got %d", resp.Total)
	}
	if len(resp.Results) != 2 {
		t.Errorf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Limit != 2 {
		t.Errorf("expected limit=2, got %d", resp.Limit)
	}

	// Offset=3, Limit=2 — should get 2 results (items 4 and 5)
	resp, err = s.Search(SearchParams{Query: "widget", Workspace: ws.Slug, Limit: 2, Offset: 3})
	if err != nil {
		t.Fatalf("offset=3, limit=2: %v", err)
	}
	if resp.Total != 5 {
		t.Errorf("expected total=5, got %d", resp.Total)
	}
	if len(resp.Results) != 2 {
		t.Errorf("expected 2 results, got %d", len(resp.Results))
	}
	if resp.Offset != 3 {
		t.Errorf("expected offset=3, got %d", resp.Offset)
	}

	// Offset beyond results — should get 0 results
	resp, err = s.Search(SearchParams{Query: "widget", Workspace: ws.Slug, Offset: 10})
	if err != nil {
		t.Fatalf("offset=10: %v", err)
	}
	if resp.Total != 5 {
		t.Errorf("expected total=5, got %d", resp.Total)
	}
	if len(resp.Results) != 0 {
		t.Errorf("expected 0 results, got %d", len(resp.Results))
	}
}

func TestSearchSorting(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	s.SeedDefaultCollections(ws.ID)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID string
	for _, c := range colls {
		if c.Slug == "tasks" {
			tasksID = c.ID
			break
		}
	}

	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Alpha gadget", Fields: `{"status":"open"}`})
	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Charlie gadget", Fields: `{"status":"open"}`})
	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Bravo gadget", Fields: `{"status":"open"}`})

	// Sort by title ascending
	resp, err := s.Search(SearchParams{Query: "gadget", Workspace: ws.Slug, Sort: "title", Order: "asc"})
	if err != nil {
		t.Fatalf("sort by title: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(resp.Results))
	}
	if resp.Results[0].Item.Title != "Alpha gadget" {
		t.Errorf("expected first='Alpha gadget', got %q", resp.Results[0].Item.Title)
	}
	if resp.Results[2].Item.Title != "Charlie gadget" {
		t.Errorf("expected last='Charlie gadget', got %q", resp.Results[2].Item.Title)
	}

	// Sort by title descending
	resp, err = s.Search(SearchParams{Query: "gadget", Workspace: ws.Slug, Sort: "title", Order: "desc"})
	if err != nil {
		t.Fatalf("sort by title desc: %v", err)
	}
	if resp.Results[0].Item.Title != "Charlie gadget" {
		t.Errorf("expected first='Charlie gadget', got %q", resp.Results[0].Item.Title)
	}
	if resp.Results[2].Item.Title != "Alpha gadget" {
		t.Errorf("expected last='Alpha gadget', got %q", resp.Results[2].Item.Title)
	}
}

func TestSearchFacets(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	s.SeedDefaultCollections(ws.ID)

	colls, _ := s.ListCollections(ws.ID)
	var tasksID, ideasID string
	for _, c := range colls {
		if c.Slug == "tasks" {
			tasksID = c.ID
		}
		if c.Slug == "ideas" {
			ideasID = c.ID
		}
	}

	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Fix auth widget", Fields: `{"status":"open","priority":"high"}`})
	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Refactor widget module", Fields: `{"status":"done","priority":"medium"}`})
	s.CreateItem(ws.ID, tasksID, models.ItemCreate{Title: "Widget tests", Fields: `{"status":"open","priority":"low"}`})
	s.CreateItem(ws.ID, ideasID, models.ItemCreate{Title: "Widget dashboard", Fields: `{"status":"new"}`})

	resp, err := s.Search(SearchParams{Query: "widget", Workspace: ws.Slug})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Facets == nil {
		t.Fatal("expected facets, got nil")
	}

	// Collection facets
	if resp.Facets.Collections["tasks"] != 3 {
		t.Errorf("expected tasks=3, got %d", resp.Facets.Collections["tasks"])
	}
	if resp.Facets.Collections["ideas"] != 1 {
		t.Errorf("expected ideas=1, got %d", resp.Facets.Collections["ideas"])
	}

	// Status facets
	if resp.Facets.Statuses["open"] != 2 {
		t.Errorf("expected open=2, got %d", resp.Facets.Statuses["open"])
	}
	if resp.Facets.Statuses["done"] != 1 {
		t.Errorf("expected done=1, got %d", resp.Facets.Statuses["done"])
	}
	if resp.Facets.Statuses["new"] != 1 {
		t.Errorf("expected new=1, got %d", resp.Facets.Statuses["new"])
	}

	// Facets should reflect full result set even with pagination
	resp, err = s.Search(SearchParams{Query: "widget", Workspace: ws.Slug, Limit: 1})
	if err != nil {
		t.Fatalf("paginated search: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Errorf("expected 1 result on page, got %d", len(resp.Results))
	}
	if resp.Facets.Collections["tasks"] != 3 {
		t.Errorf("facets should be unpaginated: expected tasks=3, got %d", resp.Facets.Collections["tasks"])
	}
}

func TestContext(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")

	createTestDoc(t, s, ws.ID, "Active Doc", "content")                                 // active
	s.CreateDocument(ws.ID, models.DocumentCreate{Title: "Draft Doc", Status: "draft"}) // draft

	// Pin one doc
	draftDocs, _ := s.ListDocuments(ws.ID, models.DocumentListParams{Status: "draft"})
	if len(draftDocs) > 0 {
		pinned := true
		s.UpdateDocument(draftDocs[0].ID, models.DocumentUpdate{Pinned: &pinned})
	}

	// Context should return active + pinned
	docs, err := s.GetContext(ws.ID, nil, true)
	if err != nil {
		t.Fatalf("GetContext error: %v", err)
	}
	if len(docs) != 2 {
		t.Errorf("expected 2 context docs, got %d", len(docs))
	}
}

func TestActivity(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Test")
	doc := createTestDoc(t, s, ws.ID, "Doc", "content")

	_, _ = s.CreateActivity(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "created",
		Actor:       "user",
		Source:      "web",
	})
	_, _ = s.CreateActivity(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  doc.ID,
		Action:      "updated",
		Actor:       "agent",
		Source:      "skill",
	})

	// Workspace activity
	activities, err := s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{})
	if err != nil {
		t.Fatalf("ListWorkspaceActivity error: %v", err)
	}
	if len(activities) != 2 {
		t.Errorf("expected 2 activities, got %d", len(activities))
	}

	// Filter by actor
	activities, _ = s.ListWorkspaceActivity(ws.ID, models.ActivityListParams{Actor: "agent"})
	if len(activities) != 1 {
		t.Errorf("expected 1 agent activity, got %d", len(activities))
	}

	// Document activity
	activities, _ = s.ListDocumentActivity(doc.ID, models.ActivityListParams{})
	if len(activities) != 2 {
		t.Errorf("expected 2 doc activities, got %d", len(activities))
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"My App", "my-app"},
		{"Phase 1: Core API", "phase-1-core-api"},
		{"Hello World!!!", "hello-world"},
		{"   spaces   ", "spaces"},
		{"UPPERCASE", "uppercase"},
		{"already-slugified", "already-slugified"},
		{"dots.and.more", "dots-and-more"},
		{"Dave's Workspace", "daves-workspace"},
		{"it's a test", "its-a-test"},
	}
	for _, tt := range tests {
		got := slugify(tt.input)
		if got != tt.expected {
			t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
