package urlimport

import (
	"encoding/json"
	"strings"
)

// DetectedType is the inferred document type returned by Detect. The
// values match the JSON `detected_type` field returned from the import
// endpoint.
type DetectedType string

const (
	// TypeOpenAPI signals an OpenAPI 3.x or Swagger 2.0 specification —
	// JSON or YAML.
	TypeOpenAPI DetectedType = "openapi"
	// TypeGeneric is the catch-all: HTML, plain text, anything non-spec.
	// Handled by the Readability/HTML→Markdown converter.
	TypeGeneric DetectedType = "generic"
)

// Detect classifies a fetched document by inspecting its Content-Type
// header and the leading bytes of its body. It returns TypeOpenAPI when
// the document is a parseable OpenAPI 3.x or Swagger 2.0 spec (JSON or
// YAML), otherwise TypeGeneric. The function never inspects more than
// the first ~64 KiB.
//
// Per PLAN-1467's converter table the only two converters that ship in
// v1 are "openapi" and "generic"; anything we can't positively identify
// as an OpenAPI spec falls back to generic on purpose.
func Detect(contentType string, body []byte) DetectedType {
	ct := canonicalMediaType(contentType)

	switch ct {
	// HTML always goes to generic — no need to JSON-sniff.
	case "text/html", "application/xhtml+xml":
		return TypeGeneric
	}

	// Explicit OpenAPI media types (rarely served, but check first).
	if ct == "application/vnd.oai.openapi+json" || ct == "application/vnd.oai.openapi" {
		return TypeOpenAPI
	}
	if ct == "application/vnd.oai.openapi+yaml" || ct == "application/x-yaml" || ct == "text/yaml" || ct == "application/yaml" {
		if isOpenAPIYAML(body) {
			return TypeOpenAPI
		}
	}

	// JSON path — sniff for top-level `openapi` / `swagger` keys.
	if ct == "application/json" || strings.HasSuffix(ct, "+json") || looksLikeJSON(body) {
		if isOpenAPIJSON(body) {
			return TypeOpenAPI
		}
	}

	// YAML path — for content-type-less or text/plain responses (common
	// when serving `openapi.yaml` from a static host) sniff the body.
	if isOpenAPIYAML(body) {
		return TypeOpenAPI
	}

	return TypeGeneric
}

// canonicalMediaType returns the lower-cased media type stripped of
// parameters (charset, boundary, etc.).
func canonicalMediaType(contentType string) string {
	if contentType == "" {
		return ""
	}
	mt := contentType
	if i := strings.Index(mt, ";"); i >= 0 {
		mt = mt[:i]
	}
	return strings.ToLower(strings.TrimSpace(mt))
}

// looksLikeJSON returns true when the body's first non-whitespace byte
// is `{` or `[`.
func looksLikeJSON(body []byte) bool {
	for _, b := range body {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// isOpenAPIJSON returns true when the body parses as a JSON object that
// declares a top-level `openapi` (3.x) or `swagger` (2.0) key. Other
// shapes (arrays, primitives) and parse errors return false.
func isOpenAPIJSON(body []byte) bool {
	head := body
	const cap = 64 * 1024
	if len(head) > cap {
		head = head[:cap]
	}
	// Try as object first.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(head, &top); err == nil {
		if _, ok := top["openapi"]; ok {
			return true
		}
		if _, ok := top["swagger"]; ok {
			return true
		}
		return false
	}
	// If parse failed because the body was truncated mid-object (likely
	// when len(body) == cap), the json error is unhelpful — fall back to
	// a YAML-style scan since the keys appear near the top of any
	// well-formed spec.
	if len(body) >= cap {
		return isOpenAPIYAML(head)
	}
	return false
}

// isOpenAPIYAML returns true when one of the first ~8 KiB of unindented
// (top-level) lines starts with `openapi:` or `swagger:`. We don't pull
// in a YAML parser because (a) this is a sniff, not a validate, and (b)
// a partial fetch can produce mid-document garbage that confuses a real
// parser but doesn't confuse a one-line prefix check.
func isOpenAPIYAML(body []byte) bool {
	const cap = 8 * 1024
	head := body
	if len(head) > cap {
		head = head[:cap]
	}
	for _, line := range strings.Split(string(head), "\n") {
		// Top-level keys are not indented; skip indented lines so a
		// nested `openapi:` value (unlikely but possible) doesn't false-
		// positive.
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "openapi:") || strings.HasPrefix(lower, "swagger:") {
			return true
		}
	}
	return false
}
