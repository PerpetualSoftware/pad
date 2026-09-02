package store

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// TestDestinationOracleClassifiesRealPostgresErrors is the test
// CheckJSONBAcceptable's doc comment promises.
//
// The whole suspect design rests on the claim that PostgreSQL can tell apart
// two values our own layers cannot, and that we can read its verdict. Both
// halves are assumptions until something runs them against a real server: the
// first about the database, the second about pgx's error TEXT, since the
// SQLSTATE is matched by string here (following isDeadlockError in this
// package). A wrong assumption on either half fails in the worst direction —
// silently accepting a value the migration will choke on.
func TestDestinationOracleClassifiesRealPostgresErrors(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("the destination oracle needs a real PostgreSQL (set PAD_TEST_POSTGRES_URL)")
	}

	esc := textguard.EscNUL
	backslash := esc[:1]

	t.Run("a clean document is accepted", func(t *testing.T) {
		// The control. Without it an oracle that refused everything would pass
		// every case below and refuse every migration.
		if err := s.CheckJSONBAcceptable(`{"a":"ordinary"}`); err != nil {
			t.Fatalf("a clean document was refused: %v", err)
		}
	})

	t.Run("a doubled-backslash literal is accepted", func(t *testing.T) {
		// The over-refusal leg, and the reason the oracle is the DESTINATION
		// rather than a broader predicate of ours. This value is in the suspect
		// list, it decodes to no NUL, and PostgreSQL stores it happily — a
		// preflight that refused it would block migrations over prose that
		// merely writes about this bug.
		doc := `{"note":"x` + backslash + esc + `y"}`
		if err := s.CheckJSONBAcceptable(doc); err != nil {
			t.Fatalf("the destination refused a harmless literal, so the preflight would over-refuse: %v", err)
		}
	})

	t.Run("a NUL behind a repeated key is refused", func(t *testing.T) {
		// The whole point. Our predicate accepts this — asserted here so the
		// case cannot silently become one we catch ourselves — and PostgreSQL
		// does not.
		doc := `{"a":"` + esc + `","a":"clean"}`
		if textguard.ParameterRefused(doc, true) {
			t.Fatal("the shared predicate now refuses this; the suspect class is obsolete (BUG-2812)")
		}
		err := s.CheckJSONBAcceptable(doc)
		if err == nil {
			t.Fatal("PostgreSQL accepted a NUL hidden behind a repeated key — the premise this whole " +
				"design rests on is wrong and the suspect path should be removed")
		}
		if !errors.Is(err, ErrNULDestinationRefused) {
			t.Fatalf("the refusal was not classified as a NUL refusal, so the preflight would report it "+
				"as an unrelated note instead of refusing: %v", err)
		}
		// And the classification came from the code we claim to match, not
		// from some other part of the message.
		if !strings.Contains(strings.ToUpper(err.Error()), "22P05") {
			t.Errorf("expected SQLSTATE 22P05 in the error text: %v", err)
		}
	})

	t.Run("a non-JSON value is refused but NOT as a NUL refusal", func(t *testing.T) {
		// The bucket separation. This value will also break the migration, but
		// for a reason outside this preflight's remit, and a NUL preflight that
		// refused on it would start blocking migrations unrelated to this bug.
		err := s.CheckJSONBAcceptable("not json at all")
		if err == nil {
			t.Fatal("a non-JSON value was accepted as jsonb")
		}
		if errors.Is(err, ErrNULDestinationRefused) {
			t.Errorf("a syntax failure was classified as a NUL refusal: %v", err)
		}
		// AND it is a completed verdict, not an unavailable check. This is the
		// real-server half of classifyDestinationError's rule: it trusts that a
		// bad VALUE produces SQLSTATE class 22, and here is a real one doing so.
		// Without this the class-22 rule would rest entirely on documentation.
		if errors.Is(err, ErrDestinationCheckUnavailable) {
			t.Errorf("a verdict about the value was classified as an unavailable check: %v", err)
		}
		if code := sqlStateOf(err); !strings.HasPrefix(code, "22") {
			t.Errorf("a malformed value produced SQLSTATE %q, not class 22 — classifyDestinationError's "+
				"rule is built on that class being what a bad value yields", code)
		}
	})
}

// TestDestinationOracleFailsClosedOnAnUnusableConnection is the test
// hasSQLState's doc comment promises, and it pins the distinction the preflight
// refuses on.
//
// "The server said no" and "the server never answered" must not look alike. The
// first is a verdict about the value; the second is a check that did not
// happen, and treating it as a pass would let the preflight promise a migration
// it never verified — the same defect the suspect class was added to correct,
// arriving by a different route.
//
// A CLOSED POOL rather than a fabricated error string, so what is measured is
// what pgx actually produces when the database cannot be reached.
func TestDestinationOracleFailsClosedOnAnUnusableConnection(t *testing.T) {
	dsn := os.Getenv("PAD_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("needs a real PostgreSQL (set PAD_TEST_POSTGRES_URL)")
	}

	dead, err := NewPostgres(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// CONTROL FIRST: while it is open, the oracle answers normally. Without
	// this, a check that returned "unavailable" for everything would pass the
	// assertion below and refuse every migration.
	if err := dead.CheckJSONBAcceptable(`{"a":"ordinary"}`); err != nil {
		t.Fatalf("the control failed before the pool was closed: %v", err)
	}

	if err := dead.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err = dead.CheckJSONBAcceptable(`{"a":"ordinary"}`)
	if err == nil {
		t.Fatal("a closed pool answered the cast successfully")
	}
	if !errors.Is(err, ErrDestinationCheckUnavailable) {
		t.Fatalf("a connection failure was not classified as an unavailable CHECK, so the preflight "+
			"would treat an unverified suspect as a pass: %v", err)
	}
	if errors.Is(err, ErrNULDestinationRefused) {
		t.Errorf("a connection failure was classified as a verdict about the value: %v", err)
	}
}

// TestClassifyDestinationErrorTreatsOperationalCodesAsUnverified is codex round
// 6's finding, and it is the fail-open one level below the one round 5 found.
//
// An operational SQLSTATE — a cancelled query, an administrator terminating the
// backend, a connection exception — is the server answering that it could not
// do the work, not a verdict about the value. Classified as a verdict, the
// preflight proceeds with an UNVERIFIED suspect, which is exactly what the
// three-way split exists to stop.
//
// SPLIT DELIBERATELY: the codes below are from PostgreSQL's error-code table
// and are formatted the way pgx renders them, because provoking an
// administrator shutdown inside a unit test is not worth it. The RENDERING half
// — that pgx really does write "(SQLSTATE nnnnn)" and that class 22 really is
// what a bad value produces — is measured against a real server in
// TestDestinationOracleClassifiesRealPostgresErrors, and the no-code half in
// TestDestinationOracleFailsClosedOnAnUnusableConnection. Neither half is
// assumed alone.
func TestClassifyDestinationErrorTreatsOperationalCodesAsUnverified(t *testing.T) {
	pgxish := func(code, text string) error {
		return errors.New("ERROR: " + text + " (SQLSTATE " + code + ")")
	}

	cases := []struct {
		name string
		err  error
		want destinationVerdict
		why  string
	}{
		{"NUL in jsonb", pgxish("22P05", "unsupported Unicode escape sequence"), destinationRefusedNUL,
			"the code the whole preflight refuses on."},
		{"NUL in text", pgxish("22021", "invalid byte sequence"), destinationRefusedNUL,
			"the text-column counterpart."},
		{"malformed JSON", pgxish("22P02", "invalid input syntax for type json"), destinationRejectedValue,
			"class 22 and not a NUL: the destination judged the VALUE, so it is reported, not refused on."},
		{"cancelled query", pgxish("57014", "canceling statement due to statement timeout"),
			destinationUnavailable,
			"THE FINDING. A timeout carries a SQLSTATE and says nothing about the value; treating it as a " +
				"verdict lets an unverified suspect through."},
		{"terminated by administrator", pgxish("57P01", "terminating connection due to administrator command"),
			destinationUnavailable, "same class of mistake, different code."},
		{"connection exception", pgxish("08006", "connection failure"), destinationUnavailable,
			"class 08 is transport, not data."},
		{"out of resources", pgxish("53200", "out of memory"), destinationUnavailable,
			"class 53 is the server's own state."},
		{"no code at all", errors.New("dial tcp: connection refused"), destinationUnavailable,
			"the driver never reached a server; pinned for real in the closed-pool test."},
		{"nil", nil, destinationUnavailable,
			"boundary: a classifier that answered 'verdict' for nil would make a successful cast look " +
				"like a rejection at any call site that forgot to check err first."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDestinationError(tc.err); got != tc.want {
				t.Errorf("%s\n  err:  %v\n  got %v, want %v", tc.why, tc.err, got, tc.want)
			}
		})
	}
}

// TestSQLStateExtractionEdges covers the parser the classification rests on.
func TestSQLStateExtractionEdges(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
		why  string
	}{
		{"pgx rendering", errors.New("ERROR: boom (SQLSTATE 22P05)"), "22P05", "the shape pgx produces."},
		{"no marker", errors.New("connection refused"), "", "nothing to extract."},
		{"truncated code", errors.New("... SQLSTATE 22"), "",
			"a marker with fewer than five characters after it must not yield a partial code that then " +
				"matches a class prefix."},
		{"lowercase marker", errors.New("... sqlstate 22p05)"), "22P05",
			"the search is case-insensitive, so the extraction must be too."},
		{"nil", nil, "", "boundary."},
		{
			// THE OFFSET BUG. U+0131 (dotless i) is two bytes and uppercases to
			// a one-byte "I", so searching an uppercased copy and slicing the
			// original takes the wrong five bytes. PostgreSQL renders messages
			// in lc_messages, so a non-English server is not hypothetical.
			name: "a message whose case mapping changes byte length",
			err:  errors.New("HATA: ge\u00e7ersiz \u0131\u0131\u0131 (SQLSTATE 22P05)"),
			want: "22P05",
			why:  "the code must survive a localised message; a shifted slice misclassifies a real NUL refusal.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sqlStateOf(tc.err); got != tc.want {
				t.Errorf("%s\n  got %q, want %q", tc.why, got, tc.want)
			}
		})
	}
}

// TestMigratedTablesCoversTheExport pins MigratedTables against the export's
// own SHAPE rather than against a reading of its SQL.
//
// The preflight refuses only on tables in that set, so a section added to
// WorkspaceExport without a matching entry would make the preflight quietly
// stop guarding a table the migration now copies — a miss, in the direction
// that ends in a half-finished migration. A source-regex over ExportWorkspace's
// queries would not catch it either: this codebase has already learned that
// multi-line and Sprintf-composed SQL are invisible to any such instrument
// (TASK-2825).
//
// Reflection over the struct is the maintainable version of the question: every
// slice section, plus the workspace row itself, must map to a listed table.
func TestMigratedTablesCoversTheExport(t *testing.T) {
	// Section field name -> the table it is read from. Adding a section to
	// WorkspaceExport fails the loop below until it is named here AND in
	// MigratedTables, which is the point.
	sectionTables := map[string]string{
		"Collections":  "collections",
		"Items":        "items",
		"Comments":     "comments",
		"ItemLinks":    "item_links",
		"ItemVersions": "item_versions",
	}

	migrated := MigratedTables()

	// The workspace row is carried by the Workspace field rather than a slice,
	// and ImportWorkspace writes it through CreateWorkspace.
	if !migrated["workspaces"] {
		t.Error("workspaces is not listed, but ImportWorkspace creates the workspace row")
	}

	typ := reflect.TypeOf(models.WorkspaceExport{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Slice {
			continue
		}
		table, known := sectionTables[f.Name]
		if !known {
			t.Errorf("WorkspaceExport has a section %q that this test does not map to a table. The "+
				"migration now copies something new: add it here AND to MigratedTables, or the "+
				"preflight stops guarding it.", f.Name)
			continue
		}
		if !migrated[table] {
			t.Errorf("export section %q reads %q, which MigratedTables does not list — the preflight "+
				"would not refuse on a NUL there", f.Name, table)
		}
	}

	// And the set contains nothing SPURIOUS, which would be an over-refusal
	// rather than a miss but is still wrong.
	for table := range migrated {
		if table == "workspaces" {
			continue
		}
		found := false
		for _, mapped := range sectionTables {
			if mapped == table {
				found = true
			}
		}
		if !found {
			t.Errorf("MigratedTables lists %q, which no export section reads; the preflight would "+
				"refuse a migration over a table it does not copy", table)
		}
	}
}
