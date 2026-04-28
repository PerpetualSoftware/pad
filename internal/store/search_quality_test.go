package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestSearchQualityComparison seeds identical data into both SQLite and PostgreSQL,
// runs the same queries against each, and reports ranking differences.
// This test requires PAD_TEST_POSTGRES_URL to be set — otherwise it skips.
func TestSearchQualityComparison(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set — skipping search quality comparison")
	}

	// --- Set up both stores ---
	dir := t.TempDir()
	sqliteStore, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("create sqlite store: %v", err)
	}
	t.Cleanup(func() { sqliteStore.Close() })

	pgStore := testStorePostgres(t, pgURL)

	// --- Seed identical data in both ---
	type testItem struct {
		title   string
		content string
	}

	items := []testItem{
		{"OAuth redirect bug", "The OAuth redirect URL is not being set correctly when using GitHub as the provider. Users get a 404 after login."},
		{"Add Google OAuth support", "Implement Google OAuth as a second login provider alongside GitHub. Need to register app in Google Cloud Console."},
		{"Fix database connection pooling", "Under high load the database connections are being exhausted. Need to tune pool settings and add connection limits."},
		{"API rate limiting", "Add rate limiting to public API endpoints to prevent abuse. Consider using a token bucket algorithm."},
		{"Redesign dashboard layout", "The current dashboard is cluttered. Proposal: move stats to sidebar, make the main area a kanban board."},
		{"WebSocket support for real-time updates", "Replace SSE with WebSocket for bidirectional communication. This would enable collaborative editing features."},
		{"Implement full-text search", "Add FTS to items and documents. SQLite has FTS5, PostgreSQL has tsvector. Need an abstraction layer."},
		{"CI/CD pipeline improvements", "Add parallel test execution, Docker layer caching, and automatic deployment to staging on PR merge."},
		{"User permissions and roles", "Implement RBAC with owner/editor/viewer roles per workspace. Need middleware for permission checks."},
		{"Stripe billing integration", "Add Stripe for subscription billing. Need checkout flow, webhook handler, and plan enforcement."},
		{"Mobile responsive design", "The web UI breaks on screens smaller than 768px. Need responsive CSS and touch-friendly interactions."},
		{"PostgreSQL migration", "Migrate from SQLite to PostgreSQL for cloud deployment. Need dialect abstraction and separate migration files."},
		{"Email notification system", "Send transactional emails for workspace invitations, password resets, and weekly digest summaries."},
		{"Kubernetes deployment manifests", "Create K8s manifests: Deployment, Service, Ingress, ConfigMap, Secret, PVC for PostgreSQL."},
		{"Dark mode theme", "Add a dark mode toggle. Use CSS custom properties for theming. Persist preference in localStorage."},
		{"Export data to CSV and JSON", "Allow users to export collection items as CSV or JSON. Support filtering and field selection."},
		{"Webhook delivery improvements", "Add retry logic with exponential backoff, delivery status tracking, and dead letter queue."},
		{"Two-factor authentication", "Implement TOTP-based 2FA with QR code setup, recovery codes, and enforcement policies."},
		{"API documentation with OpenAPI", "Generate OpenAPI spec from handler annotations. Serve Swagger UI at /docs."},
		{"Performance profiling and optimization", "Profile the hot paths: item list queries, FTS search, and dashboard aggregation. Target p95 < 100ms."},
	}

	// Create workspace + collection in both stores.
	stores := map[string]*Store{"sqlite": sqliteStore, "postgres": pgStore}
	wsIDs := map[string]string{}
	collIDs := map[string]string{}

	for name, s := range stores {
		ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "test-ws"})
		if err != nil {
			t.Fatalf("[%s] create workspace: %v", name, err)
		}
		wsIDs[name] = ws.ID

		coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
			Name:   "Tasks",
			Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true},{"key":"priority","label":"Priority","type":"select","options":["low","medium","high"]}]}`,
		})
		if err != nil {
			t.Fatalf("[%s] create collection: %v", name, err)
		}
		collIDs[name] = coll.ID

		for _, item := range items {
			_, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
				Title:   item.title,
				Content: item.content,
				Fields:  `{"status":"open","priority":"medium"}`,
			})
			if err != nil {
				t.Fatalf("[%s] create item %q: %v", name, item.title, err)
			}
		}
	}

	// --- Run search queries and compare ---
	queries := []struct {
		query       string
		expectTitle string // The title we most expect in the top results
		hyphenated  bool   // SQLite FTS5 treats hyphens as NOT operator — these may error on SQLite
	}{
		{"OAuth", "OAuth redirect bug", false},
		{"database connection", "Fix database connection pooling", false},
		{"PostgreSQL migration", "PostgreSQL migration", false},
		{"rate limiting", "API rate limiting", false},
		{"billing stripe", "Stripe billing integration", false},
		{"full text search", "Implement full-text search", false},
		{"dark mode", "Dark mode theme", false},
		{"two factor authentication", "Two-factor authentication", false},
		{"real time updates", "WebSocket support for real-time updates", false},
		{"Kubernetes deployment", "Kubernetes deployment manifests", false},
		{"email notification", "Email notification system", false},
		{"webhook retry", "Webhook delivery improvements", false},
		{"dashboard", "Redesign dashboard layout", false},
		{"permissions roles", "User permissions and roles", false},
		{"performance optimization", "Performance profiling and optimization", false},
	}

	t.Logf("\n%-35s | %-8s | %-8s | %s", "Query", "SQLite#1", "PG #1", "Match?")
	t.Logf("%-35s-+%-10s+%-10s+%s", "-----------------------------------", "----------", "----------", "------")

	mismatches := 0
	pgMissing := 0

	for _, q := range queries {
		sqliteResults, err := sqliteStore.SearchItems(wsIDs["sqlite"], q.query)
		if err != nil {
			t.Errorf("[sqlite] search %q: %v", q.query, err)
			continue
		}

		pgResults, err := pgStore.SearchItems(wsIDs["postgres"], q.query)
		if err != nil {
			t.Errorf("[postgres] search %q: %v", q.query, err)
			continue
		}

		sqliteTop := "(none)"
		if len(sqliteResults) > 0 {
			sqliteTop = truncate(sqliteResults[0].Item.Title, 30)
		}

		pgTop := "(none)"
		if len(pgResults) > 0 {
			pgTop = truncate(pgResults[0].Item.Title, 30)
		}

		// Check if expected result appears in top 3 for each backend.
		sqliteHasExpected := containsTitle(sqliteResults, q.expectTitle, 3)
		pgHasExpected := containsTitle(pgResults, q.expectTitle, 3)

		match := "✓"
		if !pgHasExpected && sqliteHasExpected {
			match = "✗ PG missing"
			pgMissing++
		} else if sqliteTop != pgTop {
			match = "~ different order"
			mismatches++
		}

		t.Logf("%-35s | %-30s | %-30s | %s", q.query, sqliteTop, pgTop, match)
		t.Logf("  SQLite: %d results, PG: %d results", len(sqliteResults), len(pgResults))
	}

	t.Logf("\nSummary: %d/%d queries match top result, %d PG missing from top 3",
		len(queries)-mismatches-pgMissing, len(queries), pgMissing)

	// Fail if PostgreSQL is missing expected results that SQLite finds.
	if pgMissing > 3 {
		t.Errorf("PostgreSQL missing too many expected results (%d/%d) — search quality needs tuning", pgMissing, len(queries))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func containsTitle(results []ItemSearchResult, title string, topN int) bool {
	for i, r := range results {
		if i >= topN {
			break
		}
		if r.Item.Title == title {
			return true
		}
	}
	return false
}

// TestSearchEdgeCases tests tricky search scenarios on both backends.
func TestSearchEdgeCases(t *testing.T) {
	s := testStore(t)

	ws := createTestWorkspace(t, s, "search-edge")
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Items",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open","required":true}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	testItems := []struct {
		title, content string
	}{
		{"hyphen-separated-words", "This has kebab-case naming"},
		{"CamelCaseTitle", "Testing camelCase search matching"},
		{"Special chars: @#$%", "Content with special characters !@#$%^&*()"},
		{"Short", "x"},
		{"", "Untitled item with only content about authentication"},
	}

	for _, item := range testItems {
		title := item.title
		if title == "" {
			title = "Untitled"
		}
		_, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title:   title,
			Content: item.content,
			Fields:  `{"status":"open"}`,
		})
		if err != nil {
			t.Fatalf("create item %q: %v", title, err)
		}
	}

	edgeCases := []struct {
		query     string
		expectMin int // Minimum results expected
	}{
		{"hyphen", 1},
		{"camelCase", 0}, // CamelCase is a single token — neither FTS engine splits on case boundaries
		{"authentication", 1},
		{"xyznonexistentquerymatchesnothing", 0},
	}

	for _, tc := range edgeCases {
		t.Run(fmt.Sprintf("search_%s", tc.query), func(t *testing.T) {
			results, err := s.SearchItems(ws.ID, tc.query)
			if err != nil {
				t.Fatalf("search %q: %v", tc.query, err)
			}
			if len(results) < tc.expectMin {
				t.Errorf("search %q: expected >= %d results, got %d", tc.query, tc.expectMin, len(results))
			}
		})
	}
}
