package artifact

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Decode parses an artifact's portable on-disk form back into an Artifact.
//
// It splits a leading "---\n ... \n---" frontmatter block from the body,
// unmarshals the frontmatter, validates pad_artifact and format_version, and
// reconstructs Fields from only the keys valid for the kind (omitting
// empty/zero values). The body is everything after the closing fence with the
// single separating blank line trimmed.
//
// CRLF line endings are normalized to LF up front so artifacts authored or
// transported on Windows decode identically — fence detection and body
// preservation both operate on canonical LF text.
func Decode(data []byte) (Artifact, error) {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	fmText, body, err := splitFrontmatter(normalized)
	if err != nil {
		return Artifact{}, err
	}

	var fm frontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return Artifact{}, fmt.Errorf("decode: unmarshal frontmatter: %w", ErrMalformed)
	}

	if _, err := FieldKeysForKind(fm.PadArtifact); err != nil {
		return Artifact{}, fmt.Errorf("decode: pad_artifact %q: %w", fm.PadArtifact, ErrUnknownKind)
	}
	if fm.FormatVersion != FormatVersion {
		return Artifact{}, fmt.Errorf("decode: format_version %d: %w", fm.FormatVersion, ErrUnsupportedVersion)
	}

	a := Artifact{
		Kind:          fm.PadArtifact,
		FormatVersion: fm.FormatVersion,
		Title:         fm.Title,
		Fields:        map[string]any{},
		Body:          body,
		Provenance:    fm.Provenance,
	}

	keys, _ := FieldKeysForKind(fm.PadArtifact)
	for _, k := range keys {
		switch k {
		case "status":
			putString(a.Fields, k, fm.Status)
		case "trigger":
			putString(a.Fields, k, fm.Trigger)
		case "scope":
			putString(a.Fields, k, fm.Scope)
		case "invocation_slug":
			putString(a.Fields, k, fm.InvocationSlug)
		case "priority":
			putString(a.Fields, k, fm.Priority)
		case "role":
			putString(a.Fields, k, fm.Role)
		case "arguments":
			if len(fm.Arguments) > 0 {
				a.Fields[k] = fm.Arguments
			}
		}
	}

	return a, nil
}

func putString(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// splitFrontmatter separates a leading "---\n...\n---" block from the body.
// Returns ErrMalformed if the opening or closing fence is missing or the
// frontmatter block is empty. The returned body has the single separating
// blank line (if present) trimmed; the rest is preserved.
func splitFrontmatter(s string) (fm, body string, err error) {
	// Require the opening fence at the very start.
	const fence = "---"
	if !strings.HasPrefix(s, fence+"\n") {
		return "", "", fmt.Errorf("decode: missing opening fence: %w", ErrMalformed)
	}
	rest := s[len(fence)+1:]

	// Find the closing fence: a line containing exactly "---".
	idx := indexClosingFence(rest)
	if idx < 0 {
		return "", "", fmt.Errorf("decode: missing closing fence: %w", ErrMalformed)
	}
	fm = rest[:idx]
	if strings.TrimSpace(fm) == "" {
		return "", "", fmt.Errorf("decode: empty frontmatter: %w", ErrMalformed)
	}

	// Advance past the closing fence line.
	after := rest[idx:]
	if nl := strings.IndexByte(after, '\n'); nl >= 0 {
		after = after[nl+1:]
	} else {
		after = ""
	}

	// Trim exactly one leading blank line between fence and body.
	after = strings.TrimPrefix(after, "\n")

	return fm, after, nil
}

// indexClosingFence returns the byte offset within s of the start of the first
// line equal to "---" (optionally with trailing whitespace), or -1.
func indexClosingFence(s string) int {
	offset := 0
	for {
		line := s[offset:]
		nl := strings.IndexByte(line, '\n')
		var cur string
		if nl >= 0 {
			cur = line[:nl]
		} else {
			cur = line
		}
		if strings.TrimRight(cur, " \t\r") == "---" {
			return offset
		}
		if nl < 0 {
			return -1
		}
		offset += nl + 1
	}
}
