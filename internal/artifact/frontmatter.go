package artifact

// frontmatter is the typed YAML frontmatter representation. Using a struct
// (not a map) guarantees stable, deterministic key order on encode.
//
// Key order on the wire MUST be:
//
//	pad_artifact, format_version, title, status, trigger, scope,
//	invocation_slug, arguments, priority, role, provenance
//
// The kind-specific field keys carry omitempty so a convention artifact does
// not emit empty playbook keys and vice-versa.
type frontmatter struct {
	PadArtifact   Kind   `yaml:"pad_artifact"`
	FormatVersion int    `yaml:"format_version"`
	Title         string `yaml:"title"`

	// Playbook keys.
	Status         string           `yaml:"status,omitempty"`
	Trigger        string           `yaml:"trigger,omitempty"`
	Scope          string           `yaml:"scope,omitempty"`
	InvocationSlug string           `yaml:"invocation_slug,omitempty"`
	Arguments      []map[string]any `yaml:"arguments,omitempty"`

	// Convention-only keys (status/trigger/scope are shared above).
	Priority string `yaml:"priority,omitempty"`
	Role     string `yaml:"role,omitempty"`

	Provenance Provenance `yaml:"provenance"`
}
