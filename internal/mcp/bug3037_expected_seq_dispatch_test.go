package mcp

import (
	"strings"
	"testing"
)

// BUG-3037 (codex round 1) — the remote dispatcher must REFUSE a fractional
// expected_seq rather than truncate it.
//
// int64(42.7) is 42, which is a different row state — and one that may well be
// the CURRENT state, so truncation turns a malformed token into a silent
// ACCEPT. That is the exact failure this token exists to remove, reintroduced by
// a coercion.
func TestMapItemUpdate_ExpectedSeq(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]any
		wantErr string
		want    any
	}{
		{name: "whole float forwards as an integer", input: map[string]any{"expected_seq": float64(42)}, want: int64(42)},
		{name: "int forwards", input: map[string]any{"expected_seq": 42}, want: int64(42)},
		{name: "fractional is refused", input: map[string]any{"expected_seq": float64(42.7)}, wantErr: "whole number"},
		{name: "absent stays absent", input: map[string]any{}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, present, err := itemExpectedSeqParam(tc.input)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected a refusal naming %q, got %v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want == nil {
				if present {
					t.Fatalf("expected_seq was invented from an absent input: %v", got)
				}
				return
			}
			if !present {
				t.Fatalf("expected_seq was dropped: input %v", tc.input)
			}
			if got != tc.want {
				t.Fatalf("expected_seq = %#v, want %#v", got, tc.want)
			}
		})
	}
}
