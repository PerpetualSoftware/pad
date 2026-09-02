package store

import (
	"errors"
	"strings"
	"testing"

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
	})
}
