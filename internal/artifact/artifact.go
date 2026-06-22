// Package artifact defines a portable, self-contained format for exporting
// and importing a single playbook or convention item as Markdown body +
// YAML frontmatter.
//
// This package is the Phase-1 core: it does encode/decode only. Server-side
// validation, forgiving coercion, slug-collision handling, and input-size
// limits live at the HTTP boundary in a later phase. Keep this package a
// clean pure parse/serialize layer with no HTTP, store, or dialect deps.
package artifact

import "errors"

// FormatVersion is the current artifact format version.
const FormatVersion = 1

// Kind identifies which item type an artifact carries.
type Kind string

const (
	// KindPlaybook is a playbook artifact.
	KindPlaybook Kind = "playbook"
	// KindConvention is a convention artifact.
	KindConvention Kind = "convention"
)

// Sentinel errors. Wrap with context via fmt.Errorf("...: %w", Err...).
var (
	// ErrMalformed indicates the frontmatter is missing or unparseable.
	ErrMalformed = errors.New("artifact: malformed frontmatter")
	// ErrUnknownKind indicates pad_artifact is not playbook or convention.
	ErrUnknownKind = errors.New("artifact: unknown kind")
	// ErrUnsupportedVersion indicates format_version is not 1.
	ErrUnsupportedVersion = errors.New("artifact: unsupported format version")
)

// Provenance records where an artifact came from.
type Provenance struct {
	Workspace     string `yaml:"workspace"`
	ExportedAt    string `yaml:"exported_at"`
	Author        string `yaml:"author"`
	FormatVersion int    `yaml:"format_version"`
}

// Artifact is a decoded export of one playbook or convention item.
type Artifact struct {
	Kind          Kind
	FormatVersion int
	Title         string
	// Fields holds the item's structured field values keyed by field key.
	Fields     map[string]any
	Body       string
	Provenance Provenance
}

// playbookFieldKeys are the item fields that ride in a playbook's frontmatter.
var playbookFieldKeys = []string{"status", "trigger", "scope", "invocation_slug", "arguments"}

// conventionFieldKeys are the item fields that ride in a convention's frontmatter.
var conventionFieldKeys = []string{"status", "trigger", "scope", "priority", "role"}

// FieldKeysForKind returns the frontmatter field keys for the given kind, or
// ErrUnknownKind if the kind is not recognized. A later phase's server code
// consumes this.
func FieldKeysForKind(k Kind) ([]string, error) {
	switch k {
	case KindPlaybook:
		out := make([]string, len(playbookFieldKeys))
		copy(out, playbookFieldKeys)
		return out, nil
	case KindConvention:
		out := make([]string, len(conventionFieldKeys))
		copy(out, conventionFieldKeys)
		return out, nil
	default:
		return nil, ErrUnknownKind
	}
}
