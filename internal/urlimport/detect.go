package urlimport

import (
	"bytes"
	"encoding/json"
	"io"
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

// isOpenAPIJSON returns true when the body is a JSON object that
// declares a top-level `openapi` (3.x) or `swagger` (2.0) key. Other
// shapes (arrays, primitives) and parse errors return false.
//
// Bodies smaller than the sniff cap are fully unmarshalled — fast and
// definitive. Larger bodies (real OpenAPI specs routinely exceed 64
// KiB) are streamed through json.Decoder, scanning only top-level
// keys; if either `openapi` or `swagger` appears we return true
// without parsing the full document. Skipping descendant values keeps
// memory bounded even for huge specs.
func isOpenAPIJSON(body []byte) bool {
	const sniffCap = 64 * 1024
	if len(body) < sniffCap {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			return false
		}
		if _, ok := top["openapi"]; ok {
			return true
		}
		_, ok := top["swagger"]
		return ok
	}
	return scanTopLevelJSONKey(body, "openapi") || scanTopLevelJSONKey(body, "swagger")
}

// scanTopLevelJSONKey returns true if `key` appears as a top-level key
// in the JSON object encoded in body. It uses json.Decoder so it can
// terminate early once the key is seen and works on streams that are
// orders of magnitude larger than the sniff cap. Truncated input
// returns false.
func scanTopLevelJSONKey(body []byte, key string) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return false
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		name, ok := tok.(string)
		if !ok {
			return false
		}
		if name == key {
			return true
		}
		if err := skipJSONValue(dec); err != nil {
			return false
		}
	}
	return false
}

// skipJSONValue advances dec past one complete JSON value (object,
// array, or scalar). Used by scanTopLevelJSONKey to land on the next
// object key without materializing the intervening value.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, isDelim := tok.(json.Delim)
	if !isDelim {
		return nil // scalar — already consumed.
	}
	if d != '{' && d != '[' {
		return io.ErrUnexpectedEOF
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if dd, ok := tok.(json.Delim); ok {
			switch dd {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
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
