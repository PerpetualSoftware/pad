package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/PerpetualSoftware/pad/internal/artifact"
)

// defaultImportArtifactMaxBytes caps a single artifact import body. A
// playbook/convention artifact is a small Markdown file (frontmatter + a
// few KB of body); 1 MiB is several orders of magnitude above any real
// artifact while still cheap to hold in memory. Overridable via
// Server.SetImportArtifactMaxBytes (PAD_IMPORT_ARTIFACT_MAX_BYTES).
const defaultImportArtifactMaxBytes int64 = 1 << 20 // 1 MiB

// YAML-bomb guard limits. The frontmatter region is parsed into a yaml.Node
// tree and walked BEFORE any struct decode so a malicious artifact can't
// blow up memory/CPU via billion-laughs (alias expansion), deeply-nested
// collections, or a huge flat node count.
const (
	// maxFrontmatterDepth bounds nesting depth. A real artifact's
	// frontmatter is shallow (top-level map + the arguments sequence of
	// small maps → depth ~4). 32 is generous headroom that still stops
	// pathological deep-nesting documents.
	maxFrontmatterDepth = 32

	// maxFrontmatterNodes bounds the total node count in the frontmatter
	// tree. Defeats a flat document with an enormous number of keys/items
	// designed to exhaust memory. A real artifact has well under 100 nodes.
	maxFrontmatterNodes = 10000

	// maxFrontmatterAliases bounds YAML alias nodes. The artifact format
	// never legitimately uses anchors/aliases, so any alias is a billion-
	// laughs signal — reject outright (cap of 0).
	maxFrontmatterAliases = 0
)

// ErrArtifactTooLarge is returned when the request body exceeds the
// configured artifact size cap.
var ErrArtifactTooLarge = errors.New("artifact import: body exceeds size limit")

// ErrArtifactUnsafeYAML is returned when the frontmatter region trips one of
// the YAML-bomb guard limits (node count, nesting depth, or anchors/aliases).
var ErrArtifactUnsafeYAML = errors.New("artifact import: frontmatter rejected by safety limits")

// parseArtifactRequest is the guarded HTTP-boundary parse used by the import
// handler. It applies three checks IN ORDER:
//
//  1. Byte cap on the raw body (http.MaxBytesReader), so an oversized body is
//     rejected before full materialization.
//  2. YAML-bomb guard: the frontmatter region is parsed into a yaml.Node tree
//     and walked, enforcing maxFrontmatterNodes / maxFrontmatterDepth /
//     maxFrontmatterAliases. This runs BEFORE the struct decode so an alias-
//     storm or deep-nesting document never reaches the expanding unmarshaler.
//  3. artifact.Decode, which produces the typed Artifact.
//
// Returns the decoded Artifact or a typed error: ErrArtifactTooLarge,
// ErrArtifactUnsafeYAML, or an artifact.* sentinel (ErrMalformed /
// ErrUnknownKind / ErrUnsupportedVersion) wrapped for context. The import
// handler maps these to HTTP statuses.
func parseArtifactRequest(w http.ResponseWriter, r *http.Request, maxBytes int64) (artifact.Artifact, error) {
	if maxBytes <= 0 {
		maxBytes = defaultImportArtifactMaxBytes
	}

	// (1) Byte cap. MaxBytesReader makes ReadAll return an error once the
	// cap is exceeded, so we never materialize an oversized body.
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	data, err := io.ReadAll(limited)
	if err != nil {
		// http.MaxBytesReader surfaces a *http.MaxBytesError when the cap
		// is hit; any read error here means the body is too big or broken.
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			return artifact.Artifact{}, ErrArtifactTooLarge
		}
		return artifact.Artifact{}, fmt.Errorf("artifact import: read body: %w", err)
	}

	// (2) YAML-bomb guard on the frontmatter region only.
	if err := guardArtifactFrontmatter(data); err != nil {
		return artifact.Artifact{}, err
	}

	// (3) Typed decode.
	art, err := artifact.Decode(data)
	if err != nil {
		return artifact.Artifact{}, err
	}
	return art, nil
}

// guardArtifactFrontmatter extracts the leading "---\n...\n---" frontmatter
// region from the artifact bytes and walks it as a yaml.Node tree to enforce
// the YAML-bomb limits. It deliberately re-parses just the frontmatter (not
// the whole document) so the guard runs before artifact.Decode's struct
// unmarshal — the unmarshal is where alias expansion would otherwise blow up.
//
// A missing/malformed fence is NOT rejected here; that's left to
// artifact.Decode so the caller gets the canonical ErrMalformed. The guard
// only fires on a present-but-dangerous frontmatter.
func guardArtifactFrontmatter(data []byte) error {
	fm, ok := extractFrontmatterRegion(string(data))
	if !ok {
		// No parseable fence — defer to artifact.Decode for the canonical
		// malformed-frontmatter error.
		return nil
	}

	var root yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &root); err != nil {
		// Unparseable YAML — defer to artifact.Decode, which wraps it as
		// ErrMalformed. The node-tree unmarshal does not expand aliases
		// (that happens at the typed-decode step), so this is safe.
		return nil
	}

	var nodes, aliases int
	if err := walkArtifactNode(&root, 0, &nodes, &aliases); err != nil {
		return err
	}
	return nil
}

// walkArtifactNode recursively walks a yaml.Node tree enforcing the depth,
// node-count, and alias limits. It increments *nodes per visited node and
// *aliases per AliasNode, and returns ErrArtifactUnsafeYAML on the first
// breach.
func walkArtifactNode(n *yaml.Node, depth int, nodes, aliases *int) error {
	if n == nil {
		return nil
	}
	if depth > maxFrontmatterDepth {
		return fmt.Errorf("%w: nesting depth exceeds %d", ErrArtifactUnsafeYAML, maxFrontmatterDepth)
	}

	*nodes++
	if *nodes > maxFrontmatterNodes {
		return fmt.Errorf("%w: node count exceeds %d", ErrArtifactUnsafeYAML, maxFrontmatterNodes)
	}

	// Reject anchors and aliases. The artifact format never uses them, so
	// their presence is a billion-laughs signal. Counting both an anchored
	// node's definition and any alias referencing it keeps the cap tight.
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		*aliases++
		if *aliases > maxFrontmatterAliases {
			return fmt.Errorf("%w: anchors/aliases are not allowed", ErrArtifactUnsafeYAML)
		}
	}

	for _, child := range n.Content {
		if err := walkArtifactNode(child, depth+1, nodes, aliases); err != nil {
			return err
		}
	}
	return nil
}

// extractFrontmatterRegion returns the YAML text between the leading "---\n"
// fence and the next line equal to "---". Returns ("", false) when the
// opening or closing fence is absent. CRLF is normalized to LF first so the
// guard sees the same canonical text artifact.Decode does.
func extractFrontmatterRegion(s string) (string, bool) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	const fence = "---"
	if !strings.HasPrefix(s, fence+"\n") {
		return "", false
	}
	rest := s[len(fence)+1:]

	offset := 0
	for {
		line := rest[offset:]
		nl := strings.IndexByte(line, '\n')
		var cur string
		if nl >= 0 {
			cur = line[:nl]
		} else {
			cur = line
		}
		if strings.TrimRight(cur, " \t\r") == fence {
			return rest[:offset], true
		}
		if nl < 0 {
			return "", false
		}
		offset += nl + 1
	}
}
