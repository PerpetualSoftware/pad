package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// BUG-2951: the prefix-grammar refusal reached the caller as
// "An internal error occurred" — a 500 with no text — because the collection
// doors mapped only conflict shapes and sent everything else to
// writeInternalError. The refusal kept its data-protection value and lost its
// entire teaching value, and an MCP agent was told worse than nothing: the
// stdio classifier reads an unrecognised message as the RETRYABLE server_error
// code, so the correct agent response was to retry a call that can never work.
//
// Every case here asserts the STATUS and the TEXT together. Either alone
// passes for the wrong reason: a 400 with an empty body is not actionable, and
// the right message under a 500 still says "the server broke".

// decodeErrorBody pulls the code + message out of the standard error envelope.
func decodeErrorBody(t *testing.T, body string) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, body)
	}
	// The door must render ValidationError.Reason, never Error(): the latter
	// carries the "store: validation" sentinel prefix and whatever any layer
	// wrapped around it. Checked HERE so every caller of this helper inherits
	// it — a mutation swapping Reason for Error() at any door survived every
	// assertion this file made about the message text, because the reason is
	// a SUBSTRING of the error, so a contains-check cannot see the difference.
	if strings.Contains(env.Error.Message, "store: validation") {
		t.Errorf("message leaks the internal sentinel prefix: %q (render ValidationError.Reason, not Error())", env.Error.Message)
	}
	return env.Error.Code, env.Error.Message
}

func TestCollectionPrefixRefusal_IsActionableAtEveryDoor(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createPath := "/api/v1/workspaces/" + slug + "/collections"

	// The message must name the RULE, not merely report a failure — that is
	// what makes a refusal teach. Asserting a substring of the store's own
	// sentence keeps this honest without pinning the whole wording.
	const rule = "must start with an uppercase letter"

	t.Run("create with an invalid prefix", func(t *testing.T) {
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":   "Widgets",
			"prefix": "ab1",
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
		}
		code, msg := decodeErrorBody(t, rr.Body.String())
		if code != "bad_request" {
			t.Errorf("code = %q, want bad_request", code)
		}
		if !strings.Contains(msg, rule) {
			t.Errorf("message = %q, want it to name the rule (%q)", msg, rule)
		}
		if !strings.Contains(msg, "ab1") {
			t.Errorf("message = %q, want it to quote the rejected value", msg)
		}
	})

	t.Run("update with an invalid prefix", func(t *testing.T) {
		// Create a collection to update, then rename its prefix badly. This is
		// the door the bug was measured on.
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{"name": "Gadgets"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("setup create failed: %d %s", rr.Code, rr.Body.String())
		}
		var created struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode created collection: %v", err)
		}

		rr = doRequest(srv, "PATCH", createPath+"/"+created.Slug, map[string]interface{}{
			"prefix": "ab1",
		})
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
		}
		code, msg := decodeErrorBody(t, rr.Body.String())
		if code != "bad_request" {
			t.Errorf("code = %q, want bad_request", code)
		}
		if !strings.Contains(msg, rule) {
			t.Errorf("message = %q, want it to name the rule (%q)", msg, rule)
		}
	})

	t.Run("a valid prefix still succeeds", func(t *testing.T) {
		// The counterfactual: without it, a door that refused EVERY prefix
		// would pass both cases above.
		rr := doRequest(srv, "POST", createPath, map[string]interface{}{
			"name":   "Sprockets",
			"prefix": "SPR1",
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rr.Code, rr.Body.String())
		}
	})
}

// TestDeleteDefaultCollection_RefusalSurvivedTheTypeSwap pins the one door that
// already answered 400 — by matching the store's prose with strings.Contains.
// The swap to the typed check must not change what a caller sees, and the
// message must still be the default-collection one rather than a generic
// validation string.
func TestDeleteDefaultCollection_RefusalSurvivedTheTypeSwap(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/collections/tasks", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	code, msg := decodeErrorBody(t, rr.Body.String())
	if code != "bad_request" {
		t.Errorf("code = %q, want bad_request", code)
	}
	if !strings.Contains(strings.ToLower(msg), "default collection") {
		t.Errorf("message = %q, want the default-collection refusal", msg)
	}
}
