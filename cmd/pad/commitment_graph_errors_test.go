package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func TestCommitmentGraphErrorMarkers(t *testing.T) {
	for _, code := range []string{"schedule_linked", "deadline_approval_required", "link_violation_ack", "commitment_changed", "link_budget", "link_edit_required", "schedule_link_not_found", "link_cycle", "link_self", "link_duplicate_incoming", "link_limit", "link_invalid", "gate_duplicate", "commitment_preview_required"} {
		t.Run(code, func(t *testing.T) {
			in := &cli.APIError{Code: code, Message: "Refused"}
			raw, _ := json.Marshal(map[string]any{"error": in})
			var out bytes.Buffer
			writeRootStructuredError(&out, in)
			if out.String() != cli.StructuredErrorMarker+string(raw)+"\n" {
				t.Fatalf("graph code lost on stdio: %q", out.String())
			}
		})
	}
	for _, field := range []string{"reason", "revision", "role", "to", "cancel_policy", "dtstart"} {
		t.Run(field, func(t *testing.T) {
			in := &cli.APIError{Code: "validation_error", Message: "Invalid", Details: json.RawMessage(`{"field":"` + field + `","code":"required"}`)}
			var out bytes.Buffer
			writeRootStructuredError(&out, in)
			raw, _ := json.Marshal(map[string]any{"error": in})
			want := cli.StructuredErrorMarker + string(raw) + "\n"
			if field == "dtstart" {
				want = ""
			}
			if out.String() != want {
				t.Fatalf("field/code details lost on stdio: %q", out.String())
			}
		})
	}
}
