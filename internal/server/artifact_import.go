package server

import (
	"encoding/json"
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

// ErrArtifactUnbindableText is returned when the artifact body is not text the
// database can be asked to store — invalid UTF-8, or carrying a NUL. It is a
// client error (400), not a 500, for the reason BUG-2782 gives: this is a
// value Pad refuses to store, so the caller sent something that cannot mean
// anything here.
//
// "Refuses", not "cannot" — the distinction matters and the earlier wording
// blurred it (codex round 20). PostgreSQL rejects a NUL in a text or JSON
// column outright; SQLite would accept one in TEXT. So this is an application
// rule Pad applies on both dialects, not a storage limit it inherits from
// either. Stating it as a capability would tell the next reader that SQLite
// enforces something it does not.
var ErrArtifactUnbindableText = errors.New("artifact import: body contains invalid UTF-8 or a NUL character")

// parseArtifactRequest is the guarded HTTP-boundary parse used by the import
// handler. It applies five steps IN ORDER:
//
//  1. Byte cap on the raw body (http.MaxBytesReader), so an oversized body is
//     rejected before full materialization.
//  2. Raw text validity (bindableText), so invalid UTF-8 or a NUL BYTE is
//     refused before anything parses it.
//  3. YAML-bomb guard: the frontmatter region is parsed into a yaml.Node tree
//     and walked, enforcing maxFrontmatterNodes / maxFrontmatterDepth /
//     maxFrontmatterAliases. This runs BEFORE the struct decode so an alias-
//     storm or deep-nesting document never reaches the expanding unmarshaler.
//  4. artifact.Decode, which produces the typed Artifact.
//  5. Decoded text validity, because YAML manufactures a NUL from \0 that
//     step 2 cannot see in the request bytes.
//
// Steps 2 and 5 arrived with BUG-2803; this list said "three checks" until
// codex round 8 pointed out it was describing the version before them.
//
// Returns the decoded Artifact or a typed error: ErrArtifactTooLarge,
// ErrArtifactUnbindableText, ErrArtifactUnsafeYAML, or an artifact.*
// sentinel (ErrMalformed /
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

	// (2) The artifact body is TEXT bound for text columns, and this
	// handler reads it directly rather than through decodeJSON, so it
	// inherits neither BUG-2803's refusal nor the path/query rule (a body is
	// neither). A raw NUL or invalid UTF-8 here reaches the store and
	// Postgres answers 22021, which the handler turns into a 500 for what is
	// a client error. Same predicate as ValidatePath and ValidateQuery.
	// Found by the codex round 3 sweep over body readers (BUG-2803).
	if !bindableText(string(data)) {
		return artifact.Artifact{}, ErrArtifactUnbindableText
	}

	// (3) YAML-bomb guard on the frontmatter region only.
	if err := guardArtifactFrontmatter(data); err != nil {
		return artifact.Artifact{}, err
	}

	// (4) Typed decode.
	art, err := artifact.Decode(data)
	if err != nil {
		return artifact.Artifact{}, err
	}

	// (5) The DECODED artifact must be bindable text too — the raw check in
	// (2) is not sufficient on its own. YAML has its own escape vocabulary:
	// a double-quoted scalar `title: "a\0b"` carries no NUL in the request
	// bytes, passes (2a), and manufactures one during the YAML decode.
	// Measured before this check: such an artifact imported 201 with a NUL in
	// the item title (codex round 4, BUG-2803). Same shape as the JSON half —
	// a value that only becomes dangerous after a SECOND parse — so it gets
	// the same answer, at the layer that can see it.
	if !artifactIsBindableText(art) {
		return artifact.Artifact{}, ErrArtifactUnbindableText
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

// artifactIsBindableText reports whether a decoded artifact carries a NUL in
// any string it would take into the store.
//
// It works by MARSHALLING the artifact and searching the result, rather than
// walking its fields by type. The hand-written walk this replaces missed two
// things a reviewer found immediately (codex round 8): Provenance, whose
// strings are rendered into a Markdown footer appended to the stored content,
// and Arguments, whose declared type is []map[string]any — a concrete slice
// type the walk's `[]any` case never matched. Both were reachable. A type
// switch over a struct that grows is a list that goes stale in silence, which
// is the same objection this file's jsonEncodedFieldKeys has to answer for
// and can only answer with a derivation test. Marshalling has no such gap:
// every exported field is covered, including ones added later.
//
// The marshalled form is then DECODED AGAIN and walked with the same
// machinery as caller data — not byte-searched for the escape sequence. An
// earlier version of this paragraph claimed a byte search was safe because
// "the ambiguity cannot arise here"; the body comment below records why that
// was simply wrong (codex round 9, re-caught contradicting itself in the
// closing rounds): a value holding the six LITERAL characters marshals to a
// doubled backslash that still contains the sequence as a substring.
//
// SCOPE, stated because marshalling hides one thing: invalid UTF-8 in a Go
// string marshals to U+FFFD rather than surviving, so this cannot detect it.
// It does not need to. Step (2a) rejects invalid UTF-8 in the request bytes
// before the decode, and YAML cannot manufacture it from valid input — its
// escapes name code points (\xff is U+00FF, a valid rune), where \0 names a
// NUL. NUL is the class that survives the decode, and it is the class this
// checks.
func artifactIsBindableText(art artifact.Artifact) bool {
	encoded, err := json.Marshal(art)
	if err != nil {
		// An artifact that cannot be marshalled cannot be reasoned about;
		// refuse rather than pass it on unexamined.
		return false
	}
	// Searching the marshalled BYTES for the escape is wrong, and this
	// function did it until codex round 9: a value holding the six LITERAL
	// characters marshals to a doubled backslash, which still contains the
	// six-character sequence as a substring, so valid content was refused.
	// That is the same doubled-backslash trap bodyDecodesNUL exists to
	// resolve — and an earlier version of this comment asserted it "cannot
	// arise here", which was simply wrong.
	//
	// So the marshalled form is decoded again and walked with the same
	// machinery, as caller data (no wire-key list applies to an artifact).
	// The round trip is what makes every field reachable without a type
	// switch; the walk is what makes the answer exact.
	var v any
	if err := json.Unmarshal(encoded, &v); err != nil {
		return false
	}
	return !valueDecodesNUL(v, true)
}
