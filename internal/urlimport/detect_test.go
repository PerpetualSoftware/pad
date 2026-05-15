package urlimport

import (
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        DetectedType
	}{
		// HTML — always generic
		{
			name:        "html",
			contentType: "text/html; charset=utf-8",
			body:        "<html><body>hello</body></html>",
			want:        TypeGeneric,
		},
		{
			name:        "xhtml",
			contentType: "application/xhtml+xml",
			body:        `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><body>hi</body></html>`,
			want:        TypeGeneric,
		},
		{
			name:        "html with body that mentions openapi",
			contentType: "text/html",
			body:        `<html><body>API: openapi: 3.0.0</body></html>`,
			want:        TypeGeneric,
		},

		// OpenAPI JSON
		{
			name:        "openapi 3.x json with header",
			contentType: "application/json",
			body:        `{"openapi":"3.0.0","info":{"title":"x","version":"1"},"paths":{}}`,
			want:        TypeOpenAPI,
		},
		{
			name:        "swagger 2.0 json with header",
			contentType: "application/json",
			body:        `{"swagger":"2.0","info":{"title":"x","version":"1"},"paths":{}}`,
			want:        TypeOpenAPI,
		},
		{
			name:        "openapi vendor media type",
			contentType: "application/vnd.oai.openapi+json",
			body:        `{}`,
			want:        TypeOpenAPI,
		},
		{
			name:        "openapi json with no content-type (sniffed)",
			contentType: "",
			body:        "  \n{\"openapi\":\"3.0.0\",\"info\":{}}",
			want:        TypeOpenAPI,
		},
		{
			name:        "openapi json with text/plain content-type (sniffed)",
			contentType: "text/plain",
			body:        `{"openapi":"3.0.0"}`,
			want:        TypeOpenAPI,
		},
		{
			name:        "json that isn't openapi",
			contentType: "application/json",
			body:        `{"foo":"bar","data":[1,2,3]}`,
			want:        TypeGeneric,
		},
		{
			name:        "json array (not an object)",
			contentType: "application/json",
			body:        `[{"openapi":"3.0.0"}]`,
			want:        TypeGeneric,
		},
		{
			name:        "json+ld passes through to sniff",
			contentType: "application/ld+json",
			body:        `{"@context":"http://schema.org"}`,
			want:        TypeGeneric,
		},

		// OpenAPI YAML
		{
			name:        "openapi 3.x yaml",
			contentType: "application/yaml",
			body:        "openapi: 3.0.0\ninfo:\n  title: Pet Store\n  version: 1.0.0\npaths: {}\n",
			want:        TypeOpenAPI,
		},
		{
			name:        "swagger 2.0 yaml",
			contentType: "text/yaml",
			body:        "swagger: \"2.0\"\ninfo:\n  title: x\n  version: 1\n",
			want:        TypeOpenAPI,
		},
		{
			name:        "openapi yaml with leading comments",
			contentType: "application/x-yaml",
			body:        "# Comment\n# Another\nopenapi: 3.0.0\n",
			want:        TypeOpenAPI,
		},
		{
			name:        "yaml that isn't openapi",
			contentType: "application/yaml",
			body:        "foo: bar\nbaz: qux\n",
			want:        TypeGeneric,
		},
		{
			name:        "openapi yaml sniffed from text/plain",
			contentType: "text/plain",
			body:        "openapi: 3.0.0\n",
			want:        TypeOpenAPI,
		},
		{
			name:        "indented openapi key does not match",
			contentType: "application/yaml",
			body:        "components:\n  openapi: 3.0.0\n",
			want:        TypeGeneric,
		},

		// Edge cases
		{
			name:        "empty body",
			contentType: "",
			body:        "",
			want:        TypeGeneric,
		},
		{
			name:        "whitespace-only body",
			contentType: "",
			body:        "   \n\t  ",
			want:        TypeGeneric,
		},
		{
			name:        "content-type with charset param",
			contentType: "application/json; charset=utf-8",
			body:        `{"openapi":"3.0.0"}`,
			want:        TypeOpenAPI,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect(tc.contentType, []byte(tc.body))
			if got != tc.want {
				t.Fatalf("Detect(%q, %q) = %q, want %q", tc.contentType, tc.body, got, tc.want)
			}
		})
	}
}

func TestDetect_LargeOpenAPIJSON(t *testing.T) {
	// Real-world OpenAPI specs frequently exceed 64 KiB. The streaming
	// scanner must find the top-level `openapi` key even when the full
	// document can't be parsed in memory at sniff time.
	var b strings.Builder
	b.WriteString(`{"openapi":"3.0.0","info":{"title":"big","version":"1"},"paths":{`)
	// Pad with many endpoints so the body is well over 64 KiB.
	for i := 0; i < 2000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		// ~120 bytes per path entry → ~240 KiB total
		b.WriteString(`"/path/`)
		b.WriteString(strings.Repeat("a", 10))
		b.WriteString(`":{"get":{"summary":"`)
		b.WriteString(strings.Repeat("x", 60))
		b.WriteString(`","responses":{"200":{"description":"ok"}}}}`)
	}
	b.WriteString("}}")
	body := []byte(b.String())
	if len(body) < 64*1024 {
		t.Fatalf("test setup: body is %d bytes, want > 64 KiB", len(body))
	}
	if got := Detect("application/json", body); got != TypeOpenAPI {
		t.Fatalf("Detect = %q, want %q for large OpenAPI JSON (%d bytes)", got, TypeOpenAPI, len(body))
	}
}

func TestDetect_LargeOpenAPIJSON_KeyNotFirst(t *testing.T) {
	// Same large body, but `openapi` is NOT the first top-level key —
	// proves the streaming scan walks past other keys to find it.
	var b strings.Builder
	b.WriteString(`{"info":{"title":"big","version":"1","description":"`)
	b.WriteString(strings.Repeat("x", 64*1024+10000))
	b.WriteString(`"},"openapi":"3.0.0","paths":{}}`)
	body := []byte(b.String())
	if len(body) < 64*1024 {
		t.Fatalf("test setup: body is %d bytes, want > 64 KiB", len(body))
	}
	if got := Detect("application/json", body); got != TypeOpenAPI {
		t.Fatalf("Detect = %q, want %q (openapi key after a huge info block)", got, TypeOpenAPI)
	}
}

func TestDetect_LargeJSONNotOpenAPI(t *testing.T) {
	// A huge JSON document that is not an OpenAPI spec must stay generic.
	var b strings.Builder
	b.WriteString(`{"name":"big","items":[`)
	for i := 0; i < 5000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":`)
		b.WriteString(strings.Repeat("9", 20))
		b.WriteString(`,"value":"x"}`)
	}
	b.WriteString(`]}`)
	body := []byte(b.String())
	if got := Detect("application/json", body); got != TypeGeneric {
		t.Fatalf("Detect = %q, want %q for large non-OpenAPI JSON", got, TypeGeneric)
	}
}

func TestCanonicalMediaType(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"text/html", "text/html"},
		{"text/html; charset=utf-8", "text/html"},
		{"TEXT/HTML", "text/html"},
		{"  application/json ", "application/json"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := canonicalMediaType(tc.in); got != tc.want {
			t.Errorf("canonicalMediaType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
