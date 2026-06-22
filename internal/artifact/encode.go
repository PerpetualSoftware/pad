package artifact

import (
	"bytes"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Encode serializes an Artifact to its portable on-disk form:
//
//	---
//	<yaml frontmatter>
//	---
//
//	<body>
//
// Output is deterministic: two Encode calls on the same Artifact produce
// byte-identical output. The body is emitted verbatim, normalized to end with
// exactly one trailing newline, separated from the closing fence by exactly
// one blank line.
func Encode(a Artifact) ([]byte, error) {
	keys, err := FieldKeysForKind(a.Kind)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	fm := frontmatter{
		PadArtifact:   a.Kind,
		FormatVersion: a.FormatVersion,
		Title:         a.Title,
		Provenance:    a.Provenance,
	}
	if fm.FormatVersion == 0 {
		fm.FormatVersion = FormatVersion
	}
	if fm.Provenance.FormatVersion == 0 {
		fm.Provenance.FormatVersion = FormatVersion
	}

	for _, k := range keys {
		v, ok := a.Fields[k]
		if !ok || v == nil {
			continue
		}
		switch k {
		case "status":
			fm.Status = toString(v)
		case "trigger":
			fm.Trigger = toString(v)
		case "scope":
			fm.Scope = toString(v)
		case "invocation_slug":
			fm.InvocationSlug = toString(v)
		case "priority":
			fm.Priority = toString(v)
		case "role":
			fm.Role = toString(v)
		case "arguments":
			fm.Arguments = normalizeArguments(v)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&fm); err != nil {
		return nil, fmt.Errorf("encode: marshal frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode: close encoder: %w", err)
	}

	body := normalizeBody(a.Body)

	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(buf.Bytes())
	out.WriteString("---\n\n")
	out.WriteString(body)
	return out.Bytes(), nil
}

// normalizeBody trims a trailing run of whitespace/newlines and ensures the
// body ends with exactly one trailing "\n". An empty body becomes "\n".
func normalizeBody(body string) string {
	trimmed := strings.TrimRight(body, "\n")
	return trimmed + "\n"
}

// toString coerces a scalar frontmatter value to string. Values originate as
// strings in practice; non-strings are rendered defensively.
func toString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// normalizeArguments coerces the arguments value into []map[string]any so it
// serializes as a stable YAML sequence of maps. Accepts []map[string]any or
// []any (e.g. JSON-decoded) whose elements are map[string]any. Anything it
// cannot interpret as a map is skipped.
func normalizeArguments(v any) []map[string]any {
	switch xs := v.(type) {
	case []map[string]any:
		if len(xs) == 0 {
			return nil
		}
		return xs
	case []any:
		out := make([]map[string]any, 0, len(xs))
		for _, el := range xs {
			if m := toStringMap(el); m != nil {
				out = append(out, m)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

func toStringMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}
