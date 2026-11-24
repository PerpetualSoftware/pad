package cli

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// nativeItem wraps *models.Item so JSON marshaling shadows models.Item's
// string-encoded Fields and Tags with native JSON values while keeping every
// other key unchanged.
type nativeItem struct {
	*models.Item
	Fields json.RawMessage `json:"fields"`
	Tags   json.RawMessage `json:"tags"`
}

// NativeItem wraps item so JSON marshaling emits `fields` as a native JSON
// object and `tags` as a native JSON array, leaving every other key unchanged.
// A nil item returns nil, which marshals as JSON `null`.
func NativeItem(item *models.Item) any {
	if item == nil {
		return nil
	}
	return nativeItem{
		Item:   item,
		Fields: NativeJSONOr(item.Fields, "{}"),
		Tags:   NativeJSONOr(item.Tags, "[]"),
	}
}

// NativeItems wraps a slice of items so JSON marshaling emits each item's
// `fields` and `tags` as native JSON. A nil or empty slice returns a non-nil
// empty slice, which marshals as `[]` rather than `null`.
func NativeItems(items []models.Item) any {
	out := make([]nativeItem, len(items))
	for i := range items {
		out[i] = nativeItem{
			Item:   &items[i],
			Fields: NativeJSONOr(items[i].Fields, "{}"),
			Tags:   NativeJSONOr(items[i].Tags, "[]"),
		}
	}
	return out
}

type rawJSONPatch struct {
	start int
	end   int
	repl  []byte
}

// RewriteRawItemJSON lexically rewrites only the `fields` and `tags` values of
// item objects reachable along path from the root of raw, preserving every
// other byte verbatim (key order at every level, large numeric literals,
// unknown keys). A path segment of "*" descends into every element of a JSON
// array; any other segment descends into that object key.
//
// Only string-valued `fields`/`tags` keys are rewritten: empty or whitespace
// strings become `{}` / `[]`; valid JSON strings are spliced in natively;
// invalid JSON strings and non-string values are left untouched. If raw is not
// valid JSON or does not match path, raw is returned unchanged.
func RewriteRawItemJSON(raw json.RawMessage, path ...string) json.RawMessage {
	if !json.Valid(raw) {
		return raw
	}
	var patches []rawJSONPatch
	walkRawJSONPath(raw, 0, path, &patches)
	if len(patches) == 0 {
		return raw
	}

	delta := 0
	for _, p := range patches {
		delta += len(p.repl) - (p.end - p.start)
	}
	out := make([]byte, 0, len(raw)+delta)
	prev := 0
	for _, p := range patches {
		out = append(out, raw[prev:p.start]...)
		out = append(out, p.repl...)
		prev = p.end
	}
	out = append(out, raw[prev:]...)
	return json.RawMessage(out)
}

func walkRawJSONPath(b []byte, pos int, path []string, patches *[]rawJSONPatch) {
	pos = skipJSONSpace(b, pos)
	if pos >= len(b) {
		return
	}
	if len(path) == 0 {
		if b[pos] != '{' {
			return
		}
		walkJSONObject(b, pos, func(key string, valStart, valEnd int) {
			var fallback string
			switch key {
			case "fields":
				fallback = "{}"
			case "tags":
				fallback = "[]"
			default:
				return
			}
			if valStart >= valEnd || b[valStart] != '"' {
				return
			}
			var s string
			if err := json.Unmarshal(b[valStart:valEnd], &s); err != nil {
				return
			}
			t := strings.TrimSpace(s)
			if t == "" {
				*patches = append(*patches, rawJSONPatch{
					start: valStart,
					end:   valEnd,
					repl:  []byte(fallback),
				})
				return
			}
			if json.Valid([]byte(t)) {
				*patches = append(*patches, rawJSONPatch{
					start: valStart,
					end:   valEnd,
					repl:  []byte(t),
				})
			}
		})
		return
	}

	seg := path[0]
	rest := path[1:]
	if seg == "*" {
		if b[pos] != '[' {
			return
		}
		i := skipJSONSpace(b, pos+1)
		for i < len(b) && b[i] != ']' {
			valStart := i
			valEnd := skipJSONValue(b, valStart)
			walkRawJSONPath(b, valStart, rest, patches)
			i = skipJSONSpace(b, valEnd)
			if i < len(b) && b[i] == ',' {
				i = skipJSONSpace(b, i+1)
			}
		}
		return
	}

	if b[pos] != '{' {
		return
	}
	walkJSONObject(b, pos, func(key string, valStart, valEnd int) {
		if key == seg {
			walkRawJSONPath(b, valStart, rest, patches)
		}
	})
}

func walkJSONObject(b []byte, pos int, fn func(key string, valStart, valEnd int)) {
	i := skipJSONSpace(b, pos+1)
	for i < len(b) && b[i] != '}' {
		if b[i] != '"' {
			return
		}
		keyStart := i
		keyEnd := skipJSONString(b, keyStart)
		i = skipJSONSpace(b, keyEnd)
		if i >= len(b) || b[i] != ':' {
			return
		}
		valStart := skipJSONSpace(b, i+1)
		valEnd := skipJSONValue(b, valStart)
		if key, ok := decodeJSONKey(b[keyStart:keyEnd]); ok {
			fn(key, valStart, valEnd)
		}
		i = skipJSONSpace(b, valEnd)
		if i < len(b) && b[i] == ',' {
			i = skipJSONSpace(b, i+1)
		}
	}
}

func decodeJSONKey(tok []byte) (string, bool) {
	if len(tok) < 2 || tok[0] != '"' || tok[len(tok)-1] != '"' {
		return "", false
	}
	inner := tok[1 : len(tok)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return string(inner), true
	}
	var s string
	if err := json.Unmarshal(tok, &s); err != nil {
		return "", false
	}
	return s, true
}

func skipJSONSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func skipJSONString(b []byte, i int) int {
	i++
	for i < len(b) {
		switch b[i] {
		case '\\':
			i += 2
		case '"':
			return i + 1
		default:
			i++
		}
	}
	return len(b)
}

func skipJSONValue(b []byte, i int) int {
	i = skipJSONSpace(b, i)
	if i >= len(b) {
		return len(b)
	}
	switch b[i] {
	case '"':
		return skipJSONString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipJSONString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(b)
	default:
		for i < len(b) {
			switch b[i] {
			case ' ', '\t', '\n', '\r', ',', '}', ']':
				return i
			}
			i++
		}
		return len(b)
	}
}
