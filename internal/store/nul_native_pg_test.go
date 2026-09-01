package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// The native-Postgres leg of the four-way differential test (DOC-2823 S1).
//
// This is the leg that says Layer A is CALIBRATED rather than merely strict.
// Postgres already refuses a NUL in text and an escape decoding to one in
// jsonb; the guard's job is to give the same answer everywhere, which means it
// must not refuse what Postgres accepts (over-refusal, breaking valid writes
// on both dialects) and must not accept what Postgres refuses (under-refusal,
// leaving the SQLite/Postgres split BUG-2831 was about).
//
// It runs against the RAW driver deliberately — no guard in the path — so what
// is measured is Postgres's own verdict, not ours reflected back.
func TestNativePostgresAgreesWithTheCorpus(t *testing.T) {
	dsn := os.Getenv("PAD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; the native-Postgres leg needs a real server")
	}

	// The raw pgx driver, NOT the guarded name. Opening the guarded one here
	// would measure the guard agreeing with itself.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw pgx: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`DROP TABLE IF EXISTS nul_differential`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	// Both column classes, so each corpus case lands on the column its own
	// classing names — the same choice the store's leg makes.
	if _, err := db.Exec(`CREATE TABLE nul_differential (id TEXT PRIMARY KEY, txt TEXT, doc JSONB)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _, _ = db.Exec(`DROP TABLE IF EXISTS nul_differential`) }()

	// KNOWN GAPS get their own leg, and it asserts POSTGRES's answer rather
	// than ours (codex round 4). Running only Corpus here meant the recorded
	// gaps were never measured against the database at all — which is how one
	// of them carried a rationale claiming Postgres agreed with us when it
	// does not.
	t.Run("known gaps against Postgres", func(t *testing.T) {
		for i, c := range textguard.KnownGaps {
			t.Run(c.Name, func(t *testing.T) {
				id := "gap-" + string(rune('a'+i%26))
				var err error
				if json.Valid([]byte(strings.TrimSpace(c.Value))) {
					_, err = db.Exec(`INSERT INTO nul_differential (id, doc) VALUES ($1, $2)`, id, c.Value)
				} else {
					_, err = db.Exec(`INSERT INTO nul_differential (id, txt) VALUES ($1, $2)`, id, c.Value)
				}
				pgRefused := err != nil
				ourAnswer := textguard.ParameterRefused(c.Value, c.IsJSON)

				// The point of a recorded gap is that WE do not refuse it. If
				// that stops being true the entry belongs in Corpus.
				if ourAnswer {
					t.Errorf("this gap has CLOSED — the guard now refuses it. Move the case into Corpus.")
				}
				// And the interesting fact is what Postgres does, because it
				// decides whether the gap is a shared blind spot or a dialect
				// split. Recorded, not asserted either way, so the test states
				// the truth without pretending we have chosen a disposition.
				t.Logf("DIALECT SPLIT MEASURED: postgres refuses=%t, this guard refuses=%t, err=%v",
					pgRefused, ourAnswer, err)
				if pgRefused && !ourAnswer {
					t.Logf("Postgres refuses what we accept: a SQLite instance stores this value and a " +
						"Postgres one cannot. That is the split BUG-2812's token-walk closes.")
				}
			})
		}
	})

	for i, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			id := "case-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)

			var err error
			if c.IsJSON {
				// The routing question is "would Postgres's jsonb parser read
				// this", and the answer is json.Valid — NOT IsJSONDocument,
				// which tests for objects and arrays only.
				//
				// Using the narrow test here sent a JSON SCALAR to the text
				// column, where Postgres of course accepted it as eight
				// ordinary characters, and the leg reported agreement about a
				// column the value would never have been bound to. Measured
				// directly, the scalar IS refused as jsonb:
				//
				//	SELECT ('"a<escape>b"')::jsonb;
				//	ERROR:  unsupported Unicode escape sequence
				//
				// A value that is not valid JSON at all still goes to the text
				// column: that would be a jsonb SYNTAX error, a different
				// verdict from the NUL rule under test.
				if json.Valid([]byte(strings.TrimSpace(c.Value))) {
					_, err = db.Exec(`INSERT INTO nul_differential (id, doc) VALUES ($1, $2)`, id, c.Value)
				} else {
					_, err = db.Exec(`INSERT INTO nul_differential (id, txt) VALUES ($1, $2)`, id, c.Value)
				}
			} else {
				_, err = db.Exec(`INSERT INTO nul_differential (id, txt) VALUES ($1, $2)`, id, c.Value)
			}

			refused := err != nil
			if refused != c.Refused {
				t.Errorf("POSTGRES refused=%t, corpus says %t — Layer A and the database disagree, which is "+
					"the dialect split this unit exists to close\nvalue: %q\nisJSON: %t\nerr: %v\nwhy this case exists: %s",
					refused, c.Refused, c.Value, c.IsJSON, err, c.Why)
			}

			// When Postgres refuses, it must be refusing for the REASON the
			// corpus claims. Without this the leg would be satisfied by any
			// error at all — a typo in the SQL would read as agreement.
			if refused && err != nil {
				msg := err.Error()
				if !strings.Contains(msg, "22021") && !strings.Contains(msg, "22P05") &&
					!strings.Contains(msg, "unsupported Unicode escape") && !strings.Contains(msg, "invalid byte sequence") &&
					!strings.Contains(msg, "NUL") && !strings.Contains(msg, "null character") {
					t.Errorf("Postgres refused, but not on the NUL rule: %v", msg)
				}
			}
		})
	}
}
