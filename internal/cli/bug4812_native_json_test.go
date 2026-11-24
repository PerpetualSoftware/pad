package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBUG4812NativeJSONOr(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		empty string
		want  string
	}{
		{
			name:  "valid object",
			in:    `{"status":"open"}`,
			empty: "{}",
			want:  `{"status":"open"}`,
		},
		{
			name:  "valid array",
			in:    `["a","b"]`,
			empty: "[]",
			want:  `["a","b"]`,
		},
		{
			name:  "trimmed valid JSON",
			in:    "  {\"a\":1}\n ",
			empty: "{}",
			want:  `{"a":1}`,
		},
		{
			name:  "empty falls back to object",
			in:    "",
			empty: "{}",
			want:  "{}",
		},
		{
			name:  "whitespace-only falls back to array",
			in:    " \t\n ",
			empty: "[]",
			want:  "[]",
		},
		{
			name:  "invalid JSON emitted as JSON string",
			in:    "{not json",
			empty: "{}",
			want:  `"{not json"`,
		},
		{
			name:  "big number digits preserved verbatim",
			in:    `{"big":12345678901234567890}`,
			empty: "{}",
			want:  `{"big":12345678901234567890}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NativeJSONOr(tc.in, tc.empty)
			if !json.Valid(got) {
				t.Fatalf("NativeJSONOr(%q, %q) produced invalid JSON: %s", tc.in, tc.empty, got)
			}
			if string(got) != tc.want {
				t.Errorf("NativeJSONOr(%q, %q) = %s, want %s", tc.in, tc.empty, got, tc.want)
			}
			// Verify marshaling inside a struct succeeds and preserves digits.
			b, err := json.Marshal(struct {
				V json.RawMessage `json:"v"`
			}{V: got})
			if err != nil {
				t.Fatalf("marshal wrapped NativeJSONOr: %v", err)
			}
			if tc.name == "big number digits preserved verbatim" && !strings.Contains(string(b), "12345678901234567890") {
				t.Errorf("marshaled output lost big number digits: %s", b)
			}
		})
	}
}
