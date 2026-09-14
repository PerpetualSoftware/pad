package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// BUG-3037 (codex round 4) — a conflict on the SEQ token must print the seq
// pair and point the retry at --expected-seq.
//
// Before this, UpdateConflictDetails only decoded the timestamp fields, so a
// seq conflict rendered two EMPTY timestamps and told the caller to "retry with
// the current timestamp" — advice for a token they did not send, while the
// actual token they need was dropped on the floor.
func renderConflict(t *testing.T, details string) string {
	t.Helper()
	apiErr := &APIError{Code: "update_conflict", Message: "TASK-1 was modified by another writer since you last read it; re-read and retry.", Details: json.RawMessage(details)}
	uc := apiErr.AsUpdateConflict()
	if uc == nil {
		t.Fatalf("AsUpdateConflict returned nil for %s", details)
	}
	var buf bytes.Buffer
	WriteUpdateConflictError(&buf, apiErr, uc)
	return buf.String()
}

func TestWriteUpdateConflictError_SeqToken(t *testing.T) {
	out := renderConflict(t, `{"ref":"TASK-1","expected_seq":41,"actual_seq":42,"conflict_type":"seq"}`)

	if !strings.Contains(out, "expected seq: 41") {
		t.Errorf("the expected seq is missing:\n%s", out)
	}
	if !strings.Contains(out, "actual seq:   42") {
		t.Errorf("the actual seq — the token to retry with — is missing:\n%s", out)
	}
	if !strings.Contains(out, "--expected-seq 42") {
		t.Errorf("the retry line does not name the flag and the value to use:\n%s", out)
	}
	// The failure this replaces: empty timestamps and timestamp advice.
	if strings.Contains(out, "expected updated_at:") {
		t.Errorf("a seq conflict printed timestamp fields the caller never sent:\n%s", out)
	}
	if strings.Contains(out, "current timestamp") {
		t.Errorf("a seq conflict sent the caller back to the WEAK token:\n%s", out)
	}
	// The structured marker still carries the whole envelope for the stdio
	// classifier, including the new keys.
	if !strings.Contains(out, StructuredErrorMarker) {
		t.Errorf("the structured marker line is gone:\n%s", out)
	}
	if !strings.Contains(out, `"actual_seq":42`) {
		t.Errorf("the marker envelope dropped actual_seq:\n%s", out)
	}
}

// The timestamp form is UNCHANGED — this is the control that the seq branch did
// not simply take over the renderer.
func TestWriteUpdateConflictError_TimestampTokenUnchanged(t *testing.T) {
	out := renderConflict(t, `{"ref":"TASK-1","expected_updated_at":"2026-09-14T01:16:11Z","actual_updated_at":"2026-09-14T01:16:12Z"}`)

	if !strings.Contains(out, "expected updated_at: 2026-09-14T01:16:11Z") {
		t.Errorf("the timestamp form lost its expected value:\n%s", out)
	}
	if !strings.Contains(out, "retry with the current timestamp") {
		t.Errorf("the timestamp form lost its retry line:\n%s", out)
	}
	if strings.Contains(out, "seq") {
		t.Errorf("a timestamp conflict mentioned seq:\n%s", out)
	}
}
