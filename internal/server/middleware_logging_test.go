package server

import "testing"

// TestRedactQueryString covers the F6 leak guard: sensitive keys (token,
// password, secret, api_key / api-key) must have their values replaced
// with REDACTED before the structured request logger writes them to
// disk. Other keys must be untouched so logs remain useful for
// diagnosis.
func TestRedactQueryString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "no sensitive keys",
			in:   "ref=TASK-5&limit=20",
			want: "ref=TASK-5&limit=20",
		},
		{
			name: "token alone",
			in:   "token=abc123",
			want: "token=REDACTED",
		},
		{
			name: "token mid-string",
			in:   "ref=TASK-5&token=abc123&limit=20",
			want: "ref=TASK-5&token=REDACTED&limit=20",
		},
		{
			name: "token at end",
			in:   "ref=TASK-5&token=abc123",
			want: "ref=TASK-5&token=REDACTED",
		},
		{
			name: "case-insensitive",
			in:   "TOKEN=abc123",
			want: "TOKEN=REDACTED",
		},
		{
			name: "password",
			in:   "password=hunter2",
			want: "password=REDACTED",
		},
		{
			name: "secret",
			in:   "secret=topsecret",
			want: "secret=REDACTED",
		},
		{
			name: "api_key underscore",
			in:   "api_key=abc",
			want: "api_key=REDACTED",
		},
		{
			name: "api-key dash",
			in:   "api-key=abc",
			want: "api-key=REDACTED",
		},
		{
			name: "apikey contiguous",
			in:   "apikey=abc",
			want: "apikey=REDACTED",
		},
		{
			name: "preserves multiple non-sensitive keys",
			in:   "ref=TASK-5&token=abc&filter=open&limit=20",
			want: "ref=TASK-5&token=REDACTED&filter=open&limit=20",
		},
		{
			name: "lookalike key not redacted",
			in:   "tokenizer=on&ref=TASK-5",
			want: "tokenizer=on&ref=TASK-5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactQueryString(tc.in)
			if got != tc.want {
				t.Fatalf("redactQueryString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
