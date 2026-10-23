package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSessionActivityMalformedBody(t *testing.T) {
	srv := testServerWithPresence(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	rr := presenceRequest(t, srv, http.MethodPost, tok.Token, presenceWorkerA, `{"state":`)
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// The state check also returns validation_error. Match the decode branch's
	// message so ignoring a decode error cannot pass this test.
	if rr.Code != http.StatusBadRequest || body.Error.Code != "validation_error" || body.Error.Message != "Invalid activity body" {
		t.Fatalf("malformed body: %d %s; want 400 validation_error: Invalid activity body", rr.Code, rr.Body.String())
	}
}

func TestSessionActivityNullTimestamp(t *testing.T) {
	srv := testServerWithPresence(t)
	_, _, tok, _ := setupWatchTestUser(t, srv)
	rr := presenceRequest(t, srv, http.MethodPost, tok.Token, presenceWorkerA, `{"state":"busy","at":null}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("null at: %d %s; want 400 validation_error", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "validation_error" {
		t.Fatalf("null at: %d %s; want 400 validation_error", rr.Code, rr.Body.String())
	}
}
