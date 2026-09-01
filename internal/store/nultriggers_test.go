package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// TestNULTriggersRefuseAnUnguardedWriter is the test S2 exists for.
//
// It writes through a RAW sqlite connection to the same database file — no
// driver guard in the path — which is precisely BUG-2813's scenario: an older
// binary, a rollback, a staged rollout, a second instance. Layer A cannot help
// there because Layer A lives in the binary. The trigger is enforced by the
// FILE, so it holds for every writer.
//
// If this passes while Layer A's tests also pass, the invariant has moved from
// being a property of our code to being a property of the data.
func TestNULTriggersRefuseAnUnguardedWriter(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only; Postgres refuses natively")
	}
	ws := createTestWorkspace(t, s, "TriggerWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Trigger subject", "body")

	// The unguarded writer. Same file, raw driver — what an old binary is.
	raw, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	// Control FIRST: the raw connection can write normally, so a refusal below
	// is the trigger and not a broken fixture.
	if _, err := raw.Exec(`UPDATE items SET content = ? WHERE id = ?`, "clean from the old binary", item.ID); err != nil {
		t.Fatalf("the unguarded writer must be able to write clean values: %v", err)
	}

	t.Run("raw NUL in a text column", func(t *testing.T) {
		_, err := raw.Exec(`UPDATE items SET content = ? WHERE id = ?`, "old"+textguard.NUL+"binary", item.ID)
		if err == nil {
			t.Fatal("an unguarded writer stored a NUL — the database does not own the invariant")
		}
		if !strings.Contains(err.Error(), "pad_nul_invariant") {
			t.Errorf("refused, but not by our trigger: %v", err)
		}
	})

	t.Run("decoded escape in a JSON column", func(t *testing.T) {
		doc := `{"status":"open","note":"x` + textguard.EscNUL + `y"}`
		_, err := raw.Exec(`UPDATE items SET fields = ? WHERE id = ?`, doc, item.ID)
		if err == nil {
			t.Fatal("an unguarded writer stored a decoded-NUL escape in a JSON column")
		}
		if !strings.Contains(err.Error(), "pad_nul_invariant") {
			t.Errorf("refused, but not by our trigger: %v", err)
		}
	})

	t.Run("decoded escape in a JSON KEY", func(t *testing.T) {
		doc := `{"k` + textguard.EscNUL + `ey":"v"}`
		if _, err := raw.Exec(`UPDATE items SET fields = ? WHERE id = ?`, doc, item.ID); err == nil {
			t.Error("a NUL in a JSON KEY was stored; json_tree exposes keys and the trigger must read them")
		}
	})

	t.Run("INSERT is covered, not only UPDATE", func(t *testing.T) {
		_, err := raw.Exec(
			`INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, '{}', '[]', '2026-01-01', '2026-01-01')`,
			"trigger-insert-probe", ws.ID, col.ID, "ins"+textguard.NUL+"title", "trigger-insert-probe", "")
		if err == nil {
			t.Error("an unguarded INSERT stored a NUL title")
		}
	})

	// The doubled-backslash control, which is the false-positive the whole
	// predicate family has to avoid. Measured literal on modernc in TASK-2824.
	t.Run("a doubled backslash is NOT refused", func(t *testing.T) {
		doc := `{"note":"x\` + textguard.EscNUL + `y"}`
		if _, err := raw.Exec(`UPDATE items SET fields = ? WHERE id = ?`, doc, item.ID); err != nil {
			t.Errorf("literal escape text was refused — the trigger has a false positive, which is the "+
				"exact trap the raw-byte pre-filter failed on: %v", err)
		}
	})

	// A JSON-classed column holding a NON-DOCUMENT must still be writable:
	// json_tree raises on invalid JSON, so the json_valid guard in the trigger
	// is load-bearing rather than defensive.
	//
	// The column is workspaces.settings, NOT items.fields, and the difference
	// is the whole point of the case. items.fields carries a JSON constraint
	// from migration 056, so a non-document write there is refused whether or
	// not our trigger exists — measured by dropping the trigger pair and
	// watching the same "malformed JSON" error. A fixture rejectable for two
	// reasons discriminates neither.
	t.Run("a JSON column with no schema constraint accepts a non-document", func(t *testing.T) {
		if _, err := raw.Exec(`UPDATE workspaces SET settings = ? WHERE id = ?`, "not json at all", ws.ID); err != nil {
			t.Errorf("a non-document value in an unconstrained JSON column was refused; without the "+
				"json_valid guard json_tree raises and every such write breaks: %v", err)
		}
	})

	// And the same column DOES refuse a real escape, so the guard above did not
	// simply disable the check.
	t.Run("the unconstrained JSON column still refuses a decoded escape", func(t *testing.T) {
		doc := `{"theme":"x` + textguard.EscNUL + `y"}`
		if _, err := raw.Exec(`UPDATE workspaces SET settings = ? WHERE id = ?`, doc, ws.ID); err == nil {
			t.Error("the json_valid guard disabled the check rather than guarding it")
		}
	})
}

// TestNULTriggersMatchTheList pins the generated migration against the Go list
// it was generated from.
//
// The migration is a checked-in artifact, so nothing stops someone editing it
// by hand or adding a column to the list without regenerating. This is what
// makes "generated from the list" a fact rather than a comment.
func TestNULTriggersMatchTheList(t *testing.T) {
	body, err := os.ReadFile("migrations/084_nul_invariant_triggers.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	re := regexp.MustCompile(`CREATE TRIGGER IF NOT EXISTS pad_nul_(\w+)_(ins|upd)\b`)
	found := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(body), -1) {
		found[m[1]+"_"+m[2]] = true
	}
	if len(found) == 0 {
		t.Fatal("no triggers found in the migration — the instrument is broken, not the file")
	}

	var missing []string
	want := 0
	for _, c := range NULProtectedColumns() {
		for _, suffix := range []string{"ins", "upd"} {
			want++
			key := c.Table + "_" + c.Column + "_" + suffix
			if !found[key] {
				missing = append(missing, key)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d trigger(s) in the list but not the migration:\n  %s\n\nRegenerate with "+
			"GEN_NUL_TRIGGERS=1 go test ./internal/store/ -run TestZZGenerateTriggerMigration",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(found) != want {
		t.Errorf("migration has %d triggers, the list implies %d — an extra trigger protects a column "+
			"nobody listed, which is as much a drift as a missing one", len(found), want)
	}
}

// TestTriggerRefusalIsIndistinguishableFromLayerA discharges Ruling 2's
// condition.
//
// The second ring — user agents, IP addresses, attachment filenames — was
// admitted to Layer B on the condition that a header-derived value hitting a
// trigger must not surface as a 500 or a broken login. It cannot, because the
// trigger's abort is classified into the SAME typed error Layer A produces, so
// the handler's existing 400 mapping catches it without knowing which layer
// refused.
//
// Reaching the trigger from a GUARDED connection needs a column Layer A does
// not inspect, and the second ring is exactly that on the CURRENT binary only
// in the sense that Layer A checks values, not columns — so the honest way to
// exercise this is to classify the driver error directly and, separately, to
// prove an unguarded write produces a message the classifier recognises.
func TestTriggerRefusalIsIndistinguishableFromLayerA(t *testing.T) {
	t.Run("a trigger abort classifies as the Layer A error", func(t *testing.T) {
		raw := errors.New("SQL logic error: pad_nul_invariant: activities.user_agent must not contain a NUL (1)")
		got := classifyTriggerRefusal(raw)
		var typed *InvalidTextParameterError
		if !errors.As(got, &typed) {
			t.Fatalf("trigger abort did not classify; got %T: %v", got, got)
		}
		if !strings.Contains(typed.Reason, "activities.user_agent") {
			t.Errorf("reason %q should name the column the rule is about", typed.Reason)
		}
	})

	t.Run("an unrelated driver error is left alone", func(t *testing.T) {
		// The control. Without it, a classifier that claimed every error would
		// pass the case above and turn every database fault into a 400.
		raw := errors.New("database is locked")
		if got := classifyTriggerRefusal(raw); got != raw {
			t.Errorf("an unrelated error was reclassified: %v", got)
		}
		if classifyTriggerRefusal(nil) != nil {
			t.Error("nil must stay nil")
		}
	})

	t.Run("the marker the classifier looks for is the one the triggers emit", func(t *testing.T) {
		// The two halves are in different files and different languages, so
		// this is the only thing keeping them in step. A migration regenerated
		// with a different message would otherwise silently stop being
		// classifiable, and every trigger refusal would become a 500.
		body, err := os.ReadFile("migrations/084_nul_invariant_triggers.sql")
		if err != nil {
			t.Fatalf("read migration: %v", err)
		}
		if !strings.Contains(string(body), nulTriggerMarker) {
			t.Fatalf("the migration does not contain %q — the classifier would never fire", nulTriggerMarker)
		}
		// And the exact shape the parser depends on.
		if !strings.Contains(string(body), nulTriggerMarker+": items.title must not") {
			t.Error("the RAISE message shape changed; classifyTriggerRefusal parses '<marker>: <table>.<col> must not'")
		}
	})
}

// TestLayerBAgreesWithTheCorpus lights the FOURTH leg of the four-way
// differential test — the one S1 built the harness for and left dark.
//
// The other three drive the same corpus through the HTTP gate (classed by
// request key), Layer A at the driver (classed by value shape), and native
// Postgres. This one drives it through a REAL SQLite write on an UNGUARDED
// connection, so what answers is the trigger and nothing else.
//
// With this leg live, one corpus is measured against four independent
// enforcers, which is what licenses two enforcement layers to coexist — the
// property DOC-2823 named as the deliverable rather than the guard itself.
func TestLayerBAgreesWithTheCorpus(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only")
	}
	ws := createTestWorkspace(t, s, "LayerBCorpus")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Corpus subject", "")

	raw, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	for _, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			// Same routing rule as the store and Postgres legs: a JSON-classed
			// value that is not valid JSON goes to the text column, because the
			// JSON column would reject it as malformed before the trigger was
			// ever consulted.
			var werr error
			if c.IsJSON && json.Valid([]byte(strings.TrimSpace(c.Value))) {
				werr = execRaw(raw, `UPDATE workspaces SET settings = ? WHERE id = ?`, c.Value, ws.ID)
			} else {
				werr = execRaw(raw, `UPDATE items SET content = ? WHERE id = ?`, c.Value, item.ID)
			}

			refused := werr != nil && strings.Contains(werr.Error(), nulTriggerMarker)
			if refused != c.Refused {
				t.Errorf("LAYER B refused=%t, corpus says %t\nvalue: %q\nisJSON: %t\nerr: %v\n"+
					"why this case exists: %s", refused, c.Refused, c.Value, c.IsJSON, werr, c.Why)
			}
			// An accepted case must actually land, not merely avoid the
			// trigger — the same assertion the store leg needed before it
			// stopped passing for the wrong reason.
			if !c.Refused && werr != nil {
				t.Errorf("expected accepted, but the write failed for another reason: %v", werr)
			}
		})
	}
}

func execRaw(db *sql.DB, q string, args ...any) error {
	_, err := db.Exec(q, args...)
	return err
}
