package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// The REPAIR legs of the four-way differential test (DOC-2823 S3).
//
// The four existing legs measure what each layer REFUSES. These measure that
// every value the repair produces is one all four layers ACCEPT — which is the
// property `pad db repair-nul` exists to deliver, and the one a repair tested
// only against its own package could satisfy while still leaving rows that the
// database, or Postgres, will not have.
//
// Three of the four live here (Layer A, Layer B, native Postgres); the HTTP
// gate's is in internal/server, beside its own corpus leg.
//
// EACH LEG DRIVES THE SAME textguard.Repair the command calls. A leg that
// repaired values with a local helper would be measuring a repair nobody ships.

// TestLayerAAcceptsEveryRepairedCorpusValue — the driver guard.
func TestLayerAAcceptsEveryRepairedCorpusValue(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RepairLayerA")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Repair subject", "")

	for _, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			repaired := textguard.Repair(c.Value, c.IsJSON)

			// Same routing rule the refusal legs use: a JSON-classed value that
			// is not valid JSON goes to the text column, or the fields column
			// rejects it as malformed before the guard is consulted and the
			// case measures SQLite's JSON parser instead.
			var err error
			if c.IsJSON && json.Valid([]byte(strings.TrimSpace(repaired))) {
				_, err = s.UpdateItem(item.ID, models_ItemUpdateFields(repaired))
			} else {
				_, err = s.UpdateItem(item.ID, models_ItemUpdateContent(repaired))
			}
			if err != nil {
				t.Fatalf("Layer A refused a REPAIRED value — the repair does not satisfy the guard it is "+
					"meant to satisfy\n  original: %q\n  repaired: %q\n  err: %v\n  why this case exists: %s",
					c.Value, repaired, err, c.Why)
			}
		})
	}
}

// TestLayerBAcceptsEveryRepairedCorpusValue — the SQLite triggers, through a
// raw handle so what is measured is the DATABASE's verdict rather than Layer
// A's reflected back.
func TestLayerBAcceptsEveryRepairedCorpusValue(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only")
	}
	ws := createTestWorkspace(t, s, "RepairLayerB")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Repair subject", "")

	raw, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	for _, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			repaired := textguard.Repair(c.Value, c.IsJSON)

			var werr error
			if c.IsJSON && json.Valid([]byte(strings.TrimSpace(repaired))) {
				werr = execRaw(raw, `UPDATE workspaces SET settings = ? WHERE id = ?`, repaired, ws.ID)
			} else {
				werr = execRaw(raw, `UPDATE items SET content = ? WHERE id = ?`, repaired, item.ID)
			}
			if werr != nil {
				t.Fatalf("Layer B refused a REPAIRED value\n  original: %q\n  repaired: %q\n  err: %v",
					c.Value, repaired, werr)
			}

			// And it PERSISTED intact, for the reason the refusal leg reads its
			// values back: a trigger that discarded the row would also produce
			// no error.
			var stored string
			var rerr error
			if c.IsJSON && json.Valid([]byte(strings.TrimSpace(repaired))) {
				rerr = raw.QueryRow(`SELECT settings FROM workspaces WHERE id = ?`, ws.ID).Scan(&stored)
			} else {
				rerr = raw.QueryRow(`SELECT content FROM items WHERE id = ?`, item.ID).Scan(&stored)
			}
			if rerr != nil {
				t.Fatalf("read back: %v", rerr)
			}
			if stored != repaired {
				t.Errorf("the repaired value did not survive the round trip\n  wrote: %q\n  read:  %q",
					repaired, stored)
			}
		})
	}
}

// TestNativePostgresAcceptsEveryRepairedCorpusValue — the leg that makes the
// repair worth running.
//
// BUG-2810's filing is that an affected workspace exports and will not import,
// and that `pad db migrate-to-pg` fails partway through the copy against
// PostgreSQL's own parser. That claim is only discharged by putting the
// repaired values in front of a real Postgres.
func TestNativePostgresAcceptsEveryRepairedCorpusValue(t *testing.T) {
	dsn := os.Getenv("PAD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; the native-Postgres leg needs a real server")
	}

	// The RAW pgx driver, not the guarded name — otherwise this measures the
	// guard agreeing with itself.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw pgx: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`DROP TABLE IF EXISTS nul_repair_differential`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.Exec(
		`CREATE TABLE nul_repair_differential (id TEXT PRIMARY KEY, txt TEXT, doc JSONB)`,
	); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _, _ = db.Exec(`DROP TABLE IF EXISTS nul_repair_differential`) }()

	for i, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			repaired := textguard.Repair(c.Value, c.IsJSON)
			id := "repaired-" + strings.ReplaceAll(c.Name, " ", "-") + "-" + string(rune('a'+i%26))

			var err error
			if c.IsJSON && json.Valid([]byte(strings.TrimSpace(repaired))) {
				_, err = db.Exec(
					`INSERT INTO nul_repair_differential (id, doc) VALUES ($1, $2)`, id, repaired)
			} else {
				_, err = db.Exec(
					`INSERT INTO nul_repair_differential (id, txt) VALUES ($1, $2)`, id, repaired)
			}
			if err != nil {
				t.Fatalf("PostgreSQL refused a REPAIRED value — the repair does not unblock the migration "+
					"it exists to unblock\n  original: %q\n  repaired: %q\n  err: %v", c.Value, repaired, err)
			}
		})
	}
}
