package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2627 part 2 — `fields_patch` is the door every user field-setter lowers
// into (`pad item update --field`, the MCP `field` param on both transports),
// and it may not write system metadata.
//
// The refusal is the easy half to test and the weak half to assert: a 400 is
// also what a request that failed for some unrelated reason returns. So every
// test here pairs the status with the thing a WRONG implementation would leave
// behind — the key present in the stored blob (CONVE-12).

// reservedPatchErrorBody pulls the error envelope out of a refusal response.
func reservedPatchErrorBody(t *testing.T, body string) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope %q: %v", body, err)
	}
	return env.Error.Code, env.Error.Message
}

// getItemFields re-reads the item over HTTP and returns its decoded fields
// blob. Deliberately a fresh GET rather than the PATCH response: the question
// these tests ask is what was PERSISTED, and a handler that answered correctly
// while writing anyway would pass an assertion made against its own reply.
func getItemFields(t *testing.T, srv *Server, wsSlug, itemSlug string) map[string]any {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/items/"+itemSlug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET item: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	return decodeItemFields(t, item.Fields)
}

func TestPatchItemFieldsPatchRefusesReservedKeys(t *testing.T) {
	// A value shaped exactly like the one that produced this bug: the
	// notes array passed as a JSON-encoded STRING, which is what
	// `--field implementation_notes=[...]` sends.
	const encodedNotes = `[{"summary":"a note","details":"body"}]`

	cases := []struct {
		name        string
		key         string
		value       any
		wantRemedy  string // substring the message must name, "" = no remedy exists
		wantNoRemed bool
	}{
		{
			name:       "implementation_notes names pad item note",
			key:        models.ItemFieldImplementationNotes,
			value:      encodedNotes,
			wantRemedy: "pad item note",
		},
		{
			name:       "decision_log names pad item decide",
			key:        models.ItemFieldDecisionLog,
			value:      `[{"decision":"do it"}]`,
			wantRemedy: "pad item decide",
		},
		{
			// convention has no user-facing write path, so the message
			// must refuse WITHOUT inventing a command to point at.
			name:        "convention refuses without inventing a remedy",
			key:         models.ItemFieldConvention,
			value:       map[string]any{"trigger": "always"},
			wantNoRemed: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testServer(t)
			slug := createWSWithCollections(t, srv)
			item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open","priority":"high"}`)

			rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
				"fields_patch": map[string]interface{}{tc.key: tc.value},
			})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("PATCH %s: expected 400, got %d: %s", tc.key, rr.Code, rr.Body.String())
			}
			code, message := reservedPatchErrorBody(t, rr.Body.String())
			if code != "validation_error" {
				t.Errorf("error code: got %q want validation_error", code)
			}
			if !strings.Contains(message, tc.key) {
				t.Errorf("message must name the refused key %q; got: %s", tc.key, message)
			}
			if tc.wantRemedy != "" && !strings.Contains(message, tc.wantRemedy) {
				t.Errorf("message must name the remedy %q (PATTE-135); got: %s", tc.wantRemedy, message)
			}
			if tc.wantNoRemed {
				// The failure this guards against is a message that sends the
				// caller to a command which cannot maintain this key.
				for _, wrong := range []string{"pad item note", "pad item decide"} {
					if strings.Contains(message, wrong) {
						t.Errorf("message for %q must not point at %q, which does not write it; got: %s",
							tc.key, wrong, message)
					}
				}
			}

			// THE LOAD-BEARING ASSERTION. A 400 alone is compatible with the
			// gate being absent and the request failing elsewhere; this is
			// what a missing gate would have LEFT BEHIND.
			got := getItemFields(t, srv, slug, item.Slug)
			if _, present := got[tc.key]; present {
				t.Fatalf("refused patch still wrote %q into the stored blob: %v", tc.key, got)
			}
			if got["status"] != "open" || got["priority"] != "high" {
				t.Errorf("refused patch disturbed the untouched keys: %v", got)
			}
		})
	}
}

// TestPatchItemFieldsPatchRefusalNamesEveryReservedKeyPresent pins the
// multi-key shape: both keys named, and in the sorted order
// items.ReservedFieldKeysIn guarantees, so the message is stable between runs.
//
// The ORDER assertion here is an echo, not the guard. With two keys and Go's
// randomized map iteration, an unsorted implementation passes it half the
// time — mutation-tested, and it survived. items.TestReservedFieldKeysIn owns
// the sort (three keys, deterministic-enough, and it does fail unsorted).
func TestPatchItemFieldsPatchRefusalNamesEveryReservedKeyPresent(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{
			models.ItemFieldImplementationNotes: "[]",
			models.ItemFieldDecisionLog:         "[]",
			"status":                            "done",
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	_, message := reservedPatchErrorBody(t, rr.Body.String())
	iDecision := strings.Index(message, models.ItemFieldDecisionLog)
	iNotes := strings.Index(message, models.ItemFieldImplementationNotes)
	if iDecision < 0 || iNotes < 0 {
		t.Fatalf("message must name both refused keys; got: %s", message)
	}
	if iDecision > iNotes {
		t.Errorf("keys must appear in sorted order (decision_log before implementation_notes); got: %s", message)
	}

	// The legitimate key in the same patch must NOT have been applied — the
	// refusal is all-or-nothing, not a partial write with a warning.
	got := getItemFields(t, srv, slug, item.Slug)
	if got["status"] != "open" {
		t.Errorf("a refused patch applied its non-reserved keys anyway: %v", got)
	}
}

// TestPatchItemFieldsPatchStillWritesGitHubPR — Codex round 3, P1.
//
// `github_pr` is the one reserved key this gate must NOT refuse. `pad github
// link` needs the caller's local git branch and the `gh` CLI, so it is excluded
// from the remote MCP surface by name, and internal/mcp/dispatch_http.go's
// noRemoteEquivalent map directs remote agents to
// `item update --field github_pr=...` as the sanctioned alternative. Refusing
// it here removed a documented capability from those agents and answered with
// a message naming a command they cannot run — a circular remedy aimed at the
// exact audience the gate was supposed to help.
//
// This asserts the WRITE LANDS, not merely that no error came back: a gate
// that refused silently would satisfy a status-only assertion.
func TestPatchItemFieldsPatchStillWritesGitHubPR(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{
			models.ItemFieldGitHubPR: map[string]any{"number": 1160, "url": "https://example.test/pr/1160"},
		},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("github_pr must remain writable through fields_patch — remote MCP has no other way to link a PR; got %d: %s",
			rr.Code, rr.Body.String())
	}
	got := getItemFields(t, srv, slug, item.Slug)
	pr, ok := got[models.ItemFieldGitHubPR].(map[string]any)
	if !ok {
		t.Fatalf("github_pr did not persist: %#v", got[models.ItemFieldGitHubPR])
	}
	if pr["url"] != "https://example.test/pr/1160" {
		t.Errorf("github_pr value not stored verbatim: %#v", pr)
	}
}

// TestPatchItemFieldsPatchRemedyIsHonestWhenAlreadyUnreadable — Codex round 1.
//
// PATTE-135 asks that a suggested remedy work in the state the caller is in.
// On an item whose stored value is ALREADY undecodable, `pad item note` refuses
// too (BUG-2627 part 3's guard), so naming it unqualified sends the caller
// round a loop: field write refused → try the note → refused → back to the
// field write. The message has to say so instead.
func TestPatchItemFieldsPatchRemedyIsHonestWhenAlreadyUnreadable(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// The TASK-860 shape: the entries array stored as a JSON-encoded STRING.
	broken := `{"status":"open","implementation_notes":"[{\"summary\":\"legacy\"}]"}`
	item := createTaskWithFields(t, srv, slug, "Legacy row", broken)

	// Precondition: the row really is in the defect state. Without this the
	// test could pass against a server that never stored the string at all.
	stored := getItemFields(t, srv, slug, item.Slug)
	if _, isString := stored[models.ItemFieldImplementationNotes].(string); !isString {
		t.Fatalf("fixture did not reproduce the defect shape: %#v", stored[models.ItemFieldImplementationNotes])
	}

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{
			models.ItemFieldImplementationNotes: `[{"summary":"my repair attempt"}]`,
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	_, message := reservedPatchErrorBody(t, rr.Body.String())

	if !strings.Contains(message, "already unreadable") {
		t.Errorf("message must say the stored value is already unreadable; got: %s", message)
	}
	if !strings.Contains(message, "pad item show") {
		t.Errorf("message must point at the one action that WORKS in this state (inspection); got: %s", message)
	}
	// The circular instruction: telling them to run the command that refuses.
	if strings.Contains(message, "Maintain implementation_notes with") {
		t.Errorf("message still prescribes the append command, which refuses on this item; got: %s", message)
	}
}

// TestPatchItemFieldsPatchRemedyStandsOnAHealthyItem is the counterpart, and
// the reason the test above proves something: on an item whose stored value is
// fine, the remedy IS `pad item note` and must still be named.
func TestPatchItemFieldsPatchRemedyStandsOnAHealthyItem(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Healthy row",
		`{"status":"open","implementation_notes":[{"id":"note-1","summary":"fine"}]}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{
			models.ItemFieldImplementationNotes: "[]",
		},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	_, message := reservedPatchErrorBody(t, rr.Body.String())
	if !strings.Contains(message, "Maintain implementation_notes with") {
		t.Errorf("a healthy item must still get the working remedy; got: %s", message)
	}
	if strings.Contains(message, "already unreadable") {
		t.Errorf("healthy item wrongly described as unreadable; got: %s", message)
	}
}

// TestPatchItemFieldsPatchAllowsOrdinaryKeys is the over-breadth control leg.
// Without it, a gate that refused EVERY fields_patch would pass every
// assertion above.
func TestPatchItemFieldsPatchAllowsOrdinaryKeys(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"status": "done"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("ordinary fields_patch: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := getItemFields(t, srv, slug, item.Slug); got["status"] != "done" {
		t.Errorf("ordinary fields_patch did not apply: %v", got)
	}
}

// TestPatchItemFullFieldsStillWritesReservedKeys is the leg that decides
// whether the gate was placed correctly, and the one most worth keeping.
//
// The full `fields` blob is how Pad's OWN writers reach these keys — `pad item
// note`, `pad item decide`, `pad github link`, and convention activation
// through ItemCreate all send one. A gate written one level too high (on both
// doors, or inside the store) would break every one of them, and no assertion
// in this file about the patch door would notice.
func TestPatchItemFullFieldsStillWritesReservedKeys(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	// The shape AppendImplementationNote produces: a real array of entries.
	full := `{"status":"open","implementation_notes":[{"id":"note-1","summary":"did a thing"}]}`
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields": full,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("full-fields write of a reserved key: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := getItemFields(t, srv, slug, item.Slug)
	notes, ok := got[models.ItemFieldImplementationNotes].([]any)
	if !ok || len(notes) != 1 {
		t.Fatalf("system writer's note did not persist through the full-fields door: %v", got)
	}
}

// TestReservedFieldTablesAreExhaustive keeps the two hand-maintained per-key
// tables in step with models.ReservedItemFieldKeys().
//
// A new reserved key that misses reservedFieldLabel renders as a raw
// snake_case key in the copy dialog; one that misses reservedFieldRemedy is
// silently fine (the empty string is a legitimate answer) — which is exactly
// why it needs a test rather than vigilance. This forces the author to make
// the "no remedy exists" call DELIBERATELY by adding the key here.
func TestReservedFieldTablesAreExhaustive(t *testing.T) {
	// Keys whose remedy is deliberately empty. Adding a key here is the
	// deliberate call, and each needs its own reason:
	//
	//   - convention — written at activation / create time, with no update
	//     path to name.
	//   - github_pr — never refused by the patch gate at all
	//     (items.PatchRefusedFieldKeysIn exempts it), so a remedy here would
	//     be advice for a rejection that cannot happen.
	noRemedy := map[string]bool{
		models.ItemFieldConvention: true,
		models.ItemFieldGitHubPR:   true,
	}

	for _, key := range models.ReservedItemFieldKeys() {
		if label := reservedFieldLabel(key); label == key {
			t.Errorf("reservedFieldLabel(%q) returns the raw key — add a human label", key)
		}
		remedy := reservedFieldRemedy(key)
		switch {
		case remedy == "" && !noRemedy[key]:
			t.Errorf("reservedFieldRemedy(%q) is empty — name the write path that maintains it, "+
				"or add the key to this test's noRemedy set to record that none exists", key)
		case remedy != "" && noRemedy[key]:
			t.Errorf("reservedFieldRemedy(%q) now returns %q — drop it from this test's noRemedy set", key, remedy)
		}
	}
}
