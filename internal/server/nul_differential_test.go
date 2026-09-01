package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// The HTTP gate's leg of the four-way differential test (DOC-2823 S1).
//
// The other legs live in internal/store (Layer A at the driver, and native
// Postgres). This one asserts the thing the design was most at risk of getting
// wrong: the gate and the store CLASS a value by different means — the gate by
// request-body KEY NAME, the store by the column a parameter is bound to — and
// nothing but a shared corpus can show the two derivations agree.
//
// If this fails while internal/store's legs pass, the layers have diverged and
// the whole cluster's defining bug has come back.

// TestHTTPGateAgreesWithTheCorpus drives every corpus case through the gate's
// own predicate, classing it the way a REQUEST would: a JSON-classed value
// arrives as the string value of a JSON-encoded field key, a text one as an
// ordinary field.
func TestHTTPGateAgreesWithTheCorpus(t *testing.T) {
	for _, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			// The gate's classing is by KEY. "fields" is a JSON-encoded field
			// key (its string value is a document something re-parses);
			// "content" is ordinary user text. Choosing the key from the
			// corpus's own IsJSON is exactly the derivation under test.
			key := "content"
			if c.IsJSON {
				key = "fields"
			}
			body, err := json.Marshal(map[string]any{key: c.Value})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			got := bodyDecodesNUL(body)
			if got != c.Refused {
				t.Errorf("gate refused=%t, corpus says %t\nkey: %s\nvalue: %q\nbody: %s\nwhy this case exists: %s",
					got, c.Refused, key, c.Value, body, c.Why)
			}
		})
	}
}

// TestStoreRefusalAnswers400 pins the status. The design's requirement is a 4xx
// naming the rule, and "not a 500" is not the same claim as "exactly 400" — the
// round-4 lesson from BUG-2803, where a >=400 assertion hid a wrong status.
//
// It drives a value the HTTP gate CANNOT catch, so the refusal genuinely comes
// from the store: a raw NUL arriving through a path the body gate does not
// scan. The gate reads JSON bodies; this goes in as a query-shaped write via
// the item update handler with the NUL already decoded server-side.
func TestStoreRefusalAnswers400(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, ws, "Subject", `{"status":"open"}`)

	// A body whose JSON is valid and whose decoded value carries a real NUL.
	// json.Marshal writes it as an escape, so this is precisely the shape the
	// gate is built to catch — which makes it the wrong probe for the store.
	// Instead, write through the store directly and assert the HANDLER's
	// mapping of the resulting error, which is what the status contract is
	// about.
	poisoned := "poisoned" + textguard.NUL + "content"
	_, err := srv.store.UpdateItem(item.ID, itemUpdateContent(poisoned))
	if err == nil {
		t.Fatal("the store accepted a NUL-bearing content write")
	}

	rr := httptest.NewRecorder()
	writeInternalError(rr, err)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want exactly 400 — a refusal that will be refused identically on retry "+
			"must not be reported as a server fault (body: %s)", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "internal_error") {
		t.Errorf("body = %s, must not be the internal_error envelope", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "NUL") {
		t.Errorf("body = %s, want it to name the rule that refused", rr.Body.String())
	}

	// CONTROL: an unrelated error still answers 500, so the mapping is
	// selective rather than turning every fault into a 400.
	rr2 := httptest.NewRecorder()
	writeInternalError(rr2, errUnrelatedForTest)
	if rr2.Code != http.StatusInternalServerError {
		t.Errorf("an unrelated error answered %d; the mapping must not swallow real faults", rr2.Code)
	}
}

var errUnrelatedForTest = errorString("some unrelated store failure")

type errorString string

func (e errorString) Error() string { return string(e) }

func itemUpdateContent(v string) (u models.ItemUpdate) { u.Content = &v; return }
