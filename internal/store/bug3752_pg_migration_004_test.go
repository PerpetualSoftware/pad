package store

import (
	"os"
	"testing"
)

// BUG-3752: Postgres migration 004 renamed doc_type 'phase-plan' to 'plan'
// with an UPDATE before dropping 001's CHECK, which rejects 'plan', so any
// database holding a phase-plan document failed 004 on every startup. The
// database is put back in 001's shape (its CHECK, a phase-plan row) and 004's
// own SQL is applied through the runner.
func TestBUG3752_PGMigration004MigratesAPhasePlanDocument(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, pgURL)
	ts := "2026-01-01T00:00:00Z"
	for _, stmt := range []string{
		`ALTER TABLE documents DROP CONSTRAINT IF EXISTS documents_doc_type_check`,
		`ALTER TABLE documents ADD CONSTRAINT documents_doc_type_check CHECK (doc_type IN ('roadmap','phase-plan','architecture','ideation','feature-spec','notes','prompt-library','reference'))`,
		`INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ('w3752', 'W', 'w3752', '` + ts + `', '` + ts + `')`,
		`INSERT INTO documents (id, workspace_id, title, slug, doc_type, created_at, updated_at) VALUES ('d3752', 'w3752', 'Doc', 'doc', 'phase-plan', '` + ts + `', '` + ts + `')`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("restore 001's shape: %v\n%s", err, stmt)
		}
	}
	data, err := pgMigrationsFS.ReadFile("pgmigrations/004_doc_type_plan.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := applyPostgresMigration(s.db, "004_doc_type_plan_bug3752.sql", string(data)); err != nil {
		t.Fatalf("004 on a database holding a phase-plan document: %v", err)
	}
	var docType string
	if err := s.db.QueryRow(`SELECT doc_type FROM documents WHERE id = 'd3752'`).Scan(&docType); err != nil || docType != "plan" {
		t.Fatalf("doc_type = %q (%v), want plan", docType, err)
	}
	if _, err := s.db.Exec(`UPDATE documents SET doc_type = 'phase-plan' WHERE id = 'd3752'`); err == nil {
		t.Error("the new CHECK still admits 'phase-plan'")
	}
}
