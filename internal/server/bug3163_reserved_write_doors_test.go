package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3163 closes the last two doors a caller's hand-written reserved metadata
// key could reach: item CREATE's `fields`, and a FULL `fields` update that
// would change (or, by omitting it, delete) a stored reserved key. Convention
// activation moves to the typed ItemCreate.Convention member.

// bug3163ReservedKeys is written out by hand rather than read from
// models.ReservedItemFieldKeys(), which is the list the gate itself consults: a
// test whose population came from the subject could not see a key dropped
// from it. TestBUG3163_ReservedKeyListIsTheOneTheGateUses pins the two
// together, so a new key fails there instead of going untested here.
var bug3163ReservedKeys = []string{"convention", "decision_log", "github_pr", "implementation_notes"}

func TestBUG3163_ReservedKeyListIsTheOneTheGateUses(t *testing.T) {
	got := models.ReservedItemFieldKeys()
	want := append([]string(nil), bug3163ReservedKeys...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reserved keys changed: models has %v, this test covers %v; add the new key's legs here", got, want)
	}
}

// bothBackends runs body against SQLite and, under `make test-pg`, Postgres.
// testServer is SQLite-only whatever gate is running, so the Postgres leg needs
// its own constructor, and it SKIPS without PAD_TEST_POSTGRES_URL.
func bothBackends(t *testing.T, body func(t *testing.T, srv *Server)) {
	t.Run("sqlite", func(t *testing.T) { body(t, testServer(t)) })
	t.Run("postgres", func(t *testing.T) {
		srv, _ := testServerPostgres(t)
		body(t, srv)
	})
}

func bug3163ItemTitles(t *testing.T, srv *Server, ws, coll string) map[string]bool {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/collections/"+coll+"/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list %s: %d %s", coll, rr.Code, rr.Body.String())
	}
	var items []models.Item
	parseJSON(t, rr, &items)
	out := map[string]bool{}
	for _, it := range items {
		out[it.Title] = true
	}
	return out
}

func bug3163StoredFields(t *testing.T, srv *Server, itemID string) map[string]any {
	t.Helper()
	it, err := srv.store.GetItem(itemID)
	if err != nil || it == nil {
		t.Fatalf("get item %s: %v", itemID, err)
	}
	return decodeItemFields(t, it.Fields)
}

// Every reserved key, in the shape a system writer would store AND in the
// string shape a `--field` / MCP `field` setter produces, is refused by
// create's `fields`, and nothing is created.
func TestBUG3163_CreateRefusesEveryReservedKey(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	structured := map[string]any{
		"convention":           map[string]any{"category": "quality", "trigger": "always"},
		"decision_log":         []any{map[string]any{"decision": "d"}},
		"github_pr":            map[string]any{"number": 7, "url": "https://github.com/o/r/pull/7"},
		"implementation_notes": []any{map[string]any{"summary": "s"}},
	}
	for _, key := range bug3163ReservedKeys {
		for _, shape := range []string{"structured", "string"} {
			var value any = structured[key]
			if shape == "string" {
				raw, _ := json.Marshal(value)
				value = string(raw)
			}
			title := "refused " + key + " " + shape
			fields, _ := json.Marshal(map[string]any{"status": "open", key: value})
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", map[string]any{
				"title":  title,
				"fields": string(fields),
			})
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s/%s: expected 400, got %d: %s", key, shape, rr.Code, rr.Body.String())
				continue
			}
			code, message := reservedPatchErrorBody(t, rr.Body.String())
			if code != "validation_error" || !strings.Contains(message, `"`+key+`"`) {
				t.Errorf("%s/%s: refusal must be validation_error naming the key; got %s: %s", key, shape, code, message)
			}
			if bug3163ItemTitles(t, srv, ws, "tasks")[title] {
				t.Errorf("%s/%s: the refused create still created an item", key, shape)
			}
		}
	}

	// Over-breadth control: the same create without a reserved key succeeds.
	// A gate refusing every create with `fields` would pass everything above.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", map[string]any{
		"title":  "ordinary create",
		"fields": `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("ordinary create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

// The typed member is how convention metadata gets in: validated, normalized,
// and stored under the reserved key. The counterfactual leg is what makes the
// stored key the witness: the same request WITHOUT the member stores no
// `convention` key, although item.Convention is non-nil there too, derived
// from the sibling trigger / scope keys (see verifyConventionLanded).
func TestBUG3163_TypedConventionIsStored(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		ws := createWSWithCollections(t, srv)
		siblings := `{"status":"active","trigger":"on-commit","scope":"all","priority":"must","category":"quality"}`

		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/conventions/items", map[string]any{
			"title":  "typed convention",
			"fields": siblings,
			"convention": map[string]any{
				"category":    "quality",
				"trigger":     "on-commit",
				"surfaces":    []string{"all", "all"},
				"enforcement": "must",
				"commands":    []string{"make test"},
			},
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("typed convention create: expected 201, got %d: %s", rr.Code, rr.Body.String())
		}
		var created models.Item
		parseJSON(t, rr, &created)
		stored := bug3163StoredFields(t, srv, created.ID)
		want := map[string]any{
			"category":    "quality",
			"trigger":     "on-commit",
			"surfaces":    []any{"all"}, // normalized: duplicates dropped
			"enforcement": "must",
			"commands":    []any{"make test"},
		}
		if !reflect.DeepEqual(stored["convention"], want) {
			t.Errorf("stored convention = %#v, want %#v", stored["convention"], want)
		}
		if created.Convention == nil || created.Convention.Enforcement != "must" {
			t.Errorf("response convention metadata = %#v", created.Convention)
		}

		rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/conventions/items", map[string]any{
			"title":  "siblings only",
			"fields": siblings,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("siblings-only create: expected 201, got %d: %s", rr.Code, rr.Body.String())
		}
		var bare models.Item
		parseJSON(t, rr, &bare)
		if _, ok := bug3163StoredFields(t, srv, bare.ID)["convention"]; ok {
			t.Errorf("a create without the typed member stored a convention key")
		}
		if bare.Convention == nil {
			t.Errorf("premise: item.Convention is expected to be derived from siblings even without the key; it was nil")
		}
	})
}

func TestBUG3163_TypedConventionRefusals(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)

	// An empty object would lower to a key the extractor reads as "no metadata".
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/conventions/items", map[string]any{
		"title":      "empty typed",
		"fields":     `{"status":"active"}`,
		"convention": map[string]any{},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty typed convention: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	// The gate reads the CALLER's fields, so a reserved key there is refused
	// even when the typed member is also sent: the member does not launder it.
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/conventions/items", map[string]any{
		"title":      "both forms",
		"fields":     `{"status":"active","convention":{"category":"x"}}`,
		"convention": map[string]any{"category": "x"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("fields convention + typed member: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	titles := bug3163ItemTitles(t, srv, ws, "conventions")
	if titles["empty typed"] || titles["both forms"] {
		t.Errorf("a refused create still created an item: %v", titles)
	}
}

// Artifact import shares createItemChecked but not handleCreateItem's gate. It
// needs none because artifact.Decode builds Fields from a fixed key list, so a
// reserved key in the frontmatter never reaches the create. This is the
// regression guard for that premise.
func TestBUG3163_ArtifactImportCannotCarryAReservedKey(t *testing.T) {
	srv := testServer(t)
	ws := createWSForTest(t, srv)

	art := "---\npad_artifact: convention\nformat_version: 1\ntitle: smuggler\nstatus: active\ntrigger: always\n" +
		"convention: {category: x, trigger: always}\n" +
		"implementation_notes: [{summary: s}]\n" +
		"decision_log: [{decision: d}]\n" +
		"github_pr: {number: 1, url: \"https://github.com/o/r/pull/1\"}\n" +
		"---\n\nbody\n"
	rr := doArtifactRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/import-artifact", []byte(art))
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Slug string `json:"slug"`
	}
	parseJSON(t, rr, &resp)
	w, _ := srv.store.GetWorkspaceBySlug(ws)
	it, err := srv.store.ResolveItem(w.ID, resp.Slug)
	if err != nil || it == nil {
		t.Fatalf("resolve imported item %q: %v", resp.Slug, err)
	}
	stored := decodeItemFields(t, it.Fields)
	if stored["trigger"] != "always" {
		t.Fatalf("premise: the ordinary frontmatter key did not land, so absence below proves nothing: %v", stored)
	}
	for _, key := range bug3163ReservedKeys {
		if _, ok := stored[key]; ok {
			t.Errorf("artifact import stored reserved key %q from frontmatter: %v", key, stored[key])
		}
	}
}

// bug3163SeededItem is a task whose stored blob holds notes and decisions,
// seeded below the write doors.
func bug3163SeededItem(t *testing.T, srv *Server, ws string) models.Item {
	t.Helper()
	return createTaskThenSeedFields(t, srv, ws, "carries history",
		`{"status":"open","implementation_notes":[{"id":"note-1","summary":"kept","created_at":"2026-09-01T00:00:00Z"}],`+
			`"decision_log":[{"id":"d-1","decision":"x","rationale":"y"}]}`)
}

func bug3163PatchFields(t *testing.T, srv *Server, ws, slug, fields string) (int, string) {
	t.Helper()
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug, map[string]any{"fields": fields})
	return rr.Code, rr.Body.String()
}

// A full `fields` blob may CARRY stored reserved metadata unchanged. The carry
// is written with its keys REORDERED relative to the seed, which is what makes
// this leg discriminate on Postgres: JSONB stores a canonical order and
// spacing, so a byte comparison against the stored row fails there, and only a
// semantic one passes.
func TestBUG3163_FullFieldsCarryIsAllowed(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		ws := createWSWithCollections(t, srv)
		item := bug3163SeededItem(t, srv, ws)

		carry := `{"decision_log":[{"rationale":"y","decision":"x","id":"d-1"}],  "status":"done",` +
			`"implementation_notes":[{"created_at":"2026-09-01T00:00:00Z","summary":"kept","id":"note-1"}]}`
		if code, body := bug3163PatchFields(t, srv, ws, item.Slug, carry); code != http.StatusOK {
			t.Fatalf("carry: expected 200, got %d: %s", code, body)
		}
		if got := bug3163StoredFields(t, srv, item.ID)["status"]; got != "done" {
			t.Errorf("carry did not apply the ordinary key: status=%v", got)
		}

		// The realistic round trip: send back exactly the blob a GET returned.
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+item.Slug, nil)
		var read models.Item
		parseJSON(t, rr, &read)
		m := decodeItemFields(t, read.Fields)
		m["status"] = "open"
		roundTrip, _ := json.Marshal(m)
		if code, body := bug3163PatchFields(t, srv, ws, item.Slug, string(roundTrip)); code != http.StatusOK {
			t.Fatalf("GET round trip: expected 200, got %d: %s", code, body)
		}
	})
}

// A differing value, an omitted stored key, and a key the item does not have
// are all refused, and the stored row is unchanged afterwards.
func TestBUG3163_FullFieldsChangeIsRefused(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		ws := createWSWithCollections(t, srv)
		item := bug3163SeededItem(t, srv, ws)
		before := bug3163StoredFields(t, srv, item.ID)

		cases := []struct {
			name, fields, wantKey, wantPhrase string
		}{
			{
				name: "differs",
				fields: `{"status":"done","decision_log":[{"id":"d-1","decision":"x","rationale":"y"}],` +
					`"implementation_notes":[{"id":"note-1","summary":"REWRITTEN","created_at":"2026-09-01T00:00:00Z"}]}`,
				wantKey: "implementation_notes", wantPhrase: "differs from the stored value",
			},
			{
				name: "omitted",
				fields: `{"status":"done",` +
					`"implementation_notes":[{"id":"note-1","summary":"kept","created_at":"2026-09-01T00:00:00Z"}]}`,
				wantKey: "decision_log", wantPhrase: "missing from the write",
			},
			{
				name: "added",
				fields: `{"status":"done","decision_log":[{"id":"d-1","decision":"x","rationale":"y"}],` +
					`"implementation_notes":[{"id":"note-1","summary":"kept","created_at":"2026-09-01T00:00:00Z"}],` +
					`"github_pr":{"number":3,"url":"https://github.com/o/r/pull/3"}}`,
				wantKey: "github_pr", wantPhrase: "differs from the stored value",
			},
		}
		for _, c := range cases {
			code, body := bug3163PatchFields(t, srv, ws, item.Slug, c.fields)
			if code != http.StatusBadRequest {
				t.Errorf("%s: expected 400, got %d: %s", c.name, code, body)
				continue
			}
			errCode, message := reservedPatchErrorBody(t, body)
			if errCode != "validation_error" || !strings.Contains(message, `"`+c.wantKey+`"`) ||
				!strings.Contains(message, c.wantPhrase) || !strings.Contains(message, "fields_patch") {
				t.Errorf("%s: refusal must name %q, say %q and offer fields_patch; got %s: %s",
					c.name, c.wantKey, c.wantPhrase, errCode, message)
			}
		}
		if after := bug3163StoredFields(t, srv, item.ID); !reflect.DeepEqual(before, after) {
			t.Errorf("a refused full-fields write changed the row:\n before %v\n after  %v", before, after)
		}

		// Control: an item with no reserved metadata takes an ordinary full blob.
		plain := createTaskWithFields(t, srv, ws, "plain", `{"status":"open"}`)
		if code, body := bug3163PatchFields(t, srv, ws, plain.Slug, `{"status":"done"}`); code != http.StatusOK {
			t.Errorf("control: ordinary full-fields write expected 200, got %d: %s", code, body)
		}
	})
}

// The comparison runs against the row as re-read UNDER THE WRITE LOCK. A note
// appended after the handler's own read, but before the update transaction,
// is stored by the time the check runs: a blob that carries only the older
// note matches what the handler saw and must still be refused, or the write
// deletes the newer note. Comparing against the handler's early read passes it.
func TestBUG3163_FullFieldsCheckUsesTheLockedRow(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := bug3163SeededItem(t, srv, ws)

	appended := `{"status":"open","implementation_notes":[` +
		`{"id":"note-1","summary":"kept","created_at":"2026-09-01T00:00:00Z"},` +
		`{"id":"note-2","summary":"landed in the window","created_at":"2026-09-02T00:00:00Z"}],` +
		`"decision_log":[{"id":"d-1","decision":"x","rationale":"y"}]}`
	fired := false
	var restore func()
	restore = srv.store.SetAfterItemPreLockReadHookForTesting(func(itemID string) {
		if itemID != item.ID || fired {
			return
		}
		fired = true
		restore() // the nested update below would fire the seam again
		seedStoredFields(t, srv, item.ID, appended)
	})
	defer restore()

	stale := `{"status":"done","decision_log":[{"id":"d-1","decision":"x","rationale":"y"}],` +
		`"implementation_notes":[{"id":"note-1","summary":"kept","created_at":"2026-09-01T00:00:00Z"}]}`
	code, body := bug3163PatchFields(t, srv, ws, item.Slug, stale)
	if !fired {
		t.Fatalf("premise: the seam never fired, so the window was never exercised")
	}
	if code != http.StatusBadRequest {
		t.Fatalf("stale blob after a concurrent append: expected 400, got %d: %s", code, body)
	}
	notes, _ := bug3163StoredFields(t, srv, item.ID)["implementation_notes"].([]any)
	if len(notes) != 2 {
		t.Errorf("the concurrent note did not survive: %v", notes)
	}
}

// The stdio MCP transport classifies a refusal by matching the CLI's stderr
// text, so these first sentences are part of the contract: a rewording that
// drops the word the classifier keys on turns a deterministic 400 into a
// retryable server_error for stdio agents. internal/mcp's
// TestReservedKeyRefusalsAgreeAcrossTransports runs the classifier on these
// same literals; this pins the literals to the builders' real output.
func TestBUG3163_RefusalTextsMatchTheStdioTable(t *testing.T) {
	const createRefusal = `"implementation_notes" is system metadata and cannot be set through an item create's fields.`
	const fullFieldsRefusal = `A full "fields" write cannot change stored system metadata: "implementation_notes" differs from the stored value.`
	if got := reservedFieldCreateMessage([]string{"implementation_notes"}); !strings.HasPrefix(got, createRefusal) {
		t.Errorf("create refusal drifted from the stdio table's literal:\n got  %s\n want prefix %s", got, createRefusal)
	}
	if got := reservedFullFieldsMessage([]string{"implementation_notes"}, nil); !strings.HasPrefix(got, fullFieldsRefusal) {
		t.Errorf("full-fields refusal drifted from the stdio table's literal:\n got  %s\n want prefix %s", got, fullFieldsRefusal)
	}
}

// A stored blob that is not a JSON object has no readable reserved metadata
// (every reader parses it as an object first), so the carry check treats it as
// empty and a full `fields` write can REPAIR it. Refusing would leave the row
// unrepairable, since the unreadable-state message points callers at exactly
// this write. A reserved key in the repair is still a SET and is refused.
func TestBUG3163_NonObjectStoredBlobIsRepairable(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createTaskThenSeedFields(t, srv, ws, "corrupt blob", `["not","an","object"]`)

	if code, body := bug3163PatchFields(t, srv, ws, item.Slug,
		`{"status":"open","implementation_notes":[{"summary":"smuggled"}]}`); code != http.StatusBadRequest {
		t.Errorf("a reserved key in the repair must still be refused: %d %s", code, body)
	}
	if code, body := bug3163PatchFields(t, srv, ws, item.Slug, `{"status":"open"}`); code != http.StatusOK {
		t.Fatalf("repairing a non-object blob: expected 200, got %d: %s", code, body)
	}
	if got := bug3163StoredFields(t, srv, item.ID)["status"]; got != "open" {
		t.Errorf("repair did not land: status=%v", got)
	}
}
