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
	// ContentState records that the BODY of this artifact was one the server
	// knew to be BEHIND the item's live collaborative document at export time
	// (BUG-3033 / BUG-3000). Same values as models.Item.ContentState, named the
	// same on purpose: a reader who learned the word from an API response should
	// not need a second lesson to read it here.
	//
	// It belongs in provenance rather than beside `title`, because it is a fact
	// about THIS EXPORT and not about the item — re-export the same item after a
	// flush and the key is gone, while nothing about the playbook or convention
	// itself changed.
	//
	// Additive and omitempty, with NO format_version bump, and the two lines
	// that make that safe are worth naming since a future key will face the same
	// question. Decode uses a plain yaml.Unmarshal with no KnownFields, so an
	// older binary reading a newer artifact IGNORES this key rather than
	// failing; and decode.go compares format_version with EXACT equality against
	// the constant, so bumping it would reject artifacts in BOTH directions for
	// what is only an added optional key. A current item's bytes are therefore
	// byte-identical to what this package emitted before — and that claim has an
	// instrument older than this change: testdata/*.golden.md pin the encoded
	// bytes exactly, and they were not touched to land this key.
	//
	// What it does NOT do: stop the import. A stale body imported into another
	// workspace becomes canonical there, in a workspace whose op-log never held
	// the real content and so can never catch up — that is BUG-3032's open
	// decision (signal / refuse / accept) for the bundle format. This key is the
	// cheapest of those three on this format, and forecloses neither of the
	// others: if that unit rules refuse-or-warn for portable formats, this door
	// joins the ruling and this marker is what makes the check possible.
	ContentState string `yaml:"content_state,omitempty"`
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
