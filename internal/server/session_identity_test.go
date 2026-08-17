package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSanitizeSessionLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary basename", "docapp", "docapp"},
		{"empty", "", ""},
		{"surrounding whitespace trimmed", "  docapp  ", "docapp"},
		{"inner whitespace collapsed", "my   project", "my project"},
		{"tabs and newlines are whitespace", "my\tproject\nhere", "my project here"},
		{
			// The reason control characters are dropped rather than
			// escaped: this label is printed in a terminal (`pad`
			// listing sessions) as well as a browser, and an escape
			// sequence there rewrites what the reader sees.
			"ansi escape stripped to its printable residue",
			"doc\x1b[31mapp",
			"doc[31mapp",
		},
		{"nul and bell dropped without splitting the word", "do\x00cap\ap", "docapp"},
		{"control-only input yields empty, not garbage", "\x00\x01\x02", ""},
		{"non-ascii printable is kept — a directory can be named anything", "проект", "проект"},
		{"emoji is printable and kept", "docapp 🪶", "docapp 🪶"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeSessionLabel(tc.in); got != tc.want {
				t.Fatalf("sanitizeSessionLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeSessionLabel_TruncatesByRunes pins the bound as RUNES,
// not bytes. A byte-based cap would slice a multibyte character in half
// and emit invalid UTF-8 into a JSON response — the kind of thing that
// surfaces as a broken UI far from here.
func TestSanitizeSessionLabel_TruncatesByRunes(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", maxSessionLabelLen+10)
	got := sanitizeSessionLabel(long)
	if len([]rune(got)) != maxSessionLabelLen {
		t.Fatalf("expected %d runes, got %d", maxSessionLabelLen, len([]rune(got)))
	}

	// Each 'é' is 2 bytes: a byte-based cap would cut one in half.
	multi := strings.Repeat("é", maxSessionLabelLen+10)
	got = sanitizeSessionLabel(multi)
	if len([]rune(got)) != maxSessionLabelLen {
		t.Fatalf("expected %d runes for multibyte input, got %d", maxSessionLabelLen, len([]rune(got)))
	}
	if !isValidUTF8(got) {
		t.Fatalf("truncation produced invalid UTF-8: %q", got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestParseSessionPID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want int
	}{
		{"1234", 1234},
		{" 1234 ", 1234},
		{"", 0},
		{"0", 0},
		{"-5", 0},
		{"not-a-pid", 0},
		{"12.5", 0},
		{"1234abc", 0},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := parseSessionPID(tc.in); got != tc.want {
				t.Fatalf("parseSessionPID(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseSessionIdentity_ReadsBothHeaders is the wiring check: the
// right header feeds the right field. Cheap, and it is what catches a
// copy-paste that reads the label header into the pid slot — a mistake
// no amount of sanitizer testing would find.
func TestParseSessionIdentity_ReadsBothHeaders(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)
	r.Header.Set(sessionLabelHeader, " docapp\x00 ")
	r.Header.Set(sessionPIDHeader, "4242")

	ident := parseSessionIdentity(r)
	if ident.Label != "docapp" {
		t.Fatalf("label = %q, want %q", ident.Label, "docapp")
	}
	if ident.PID != 4242 {
		t.Fatalf("pid = %d, want 4242", ident.PID)
	}
}

// TestParseSessionIdentity_AbsentHeadersAreTheS1Shape covers the
// compatibility leg that matters most: a pre-S2 client (or any client
// that just doesn't say) must produce exactly the zero identity, which
// is the unlabelled session S1 registered. Presence is a convenience;
// nothing about a missing header may affect the connection.
func TestParseSessionIdentity_AbsentHeadersAreTheS1Shape(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)
	if ident := parseSessionIdentity(r); ident != (SessionIdentity{}) {
		t.Fatalf("expected the zero identity for a client that sends no headers, got %+v", ident)
	}
}

// TestParseSessionIdentity_GarbageDegradesRatherThanRejects pins the
// no-fail contract. There is no error return by design: a client with a
// mangled header still deserves its events, so every bad input has to
// land on the unlabelled shape rather than anywhere else.
func TestParseSessionIdentity_GarbageDegradesRatherThanRejects(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)
	r.Header.Set(sessionLabelHeader, "\x00\x01")
	r.Header.Set(sessionPIDHeader, "definitely-not-a-pid")

	if ident := parseSessionIdentity(r); ident != (SessionIdentity{}) {
		t.Fatalf("expected garbage to degrade to the zero identity, got %+v", ident)
	}
}

// TestParseSessionIdentity_ArmedQueryParam is PLAN-2613 S1's wiring
// check for the `armed` query param — deliberately a query param, not a
// third header, per session_identity.go's doc comment. Only the exact
// "true" string counts; every other value, including absence, is the
// legacy/unarmed shape (mirroring TestParseSessionIdentity_AbsentHeadersAreTheS1Shape's
// posture that a client saying nothing gets the unarmed default, not an
// error).
func TestParseSessionIdentity_ArmedQueryParam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"exact true", "armed=true", true},
		{"absent", "", false},
		{"false", "armed=false", false},
		{"empty value", "armed=", false},
		{"wrong case", "armed=True", false},
		{"truthy-but-not-true", "armed=1", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			url := "/api/v1/events/stream"
			if tc.query != "" {
				url += "?" + tc.query
			}
			r := httptest.NewRequest(http.MethodGet, url, nil)
			if got := parseSessionIdentity(r).Armed; got != tc.want {
				t.Fatalf("parseSessionIdentity(%q).Armed = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}
