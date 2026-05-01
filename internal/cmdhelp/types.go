// Package cmdhelp implements the cmdhelp v0.1 wire format
// (https://getpad.dev/cmdhelp; IDEA-927) for the `pad` CLI.
//
// The package walks a cobra command tree and emits a Document conforming
// to schema/cmdhelp.schema.json. JSON serialization lives in json.go;
// markdown serialization arrives in TASK-935.
package cmdhelp

import "encoding/json"

// Version is the cmdhelp wire-format version this package emits.
// MAJOR.MINOR only — never includes a PATCH component (spec §9).
const Version = "0.1"

// Document is the top-level cmdhelp envelope. JSON-serializable; field
// tags match schema/cmdhelp.schema.json exactly. Optional fields use
// `omitempty` so the emitter does not produce noisy null/empty keys.
type Document struct {
	CmdhelpVersion string                     `json:"cmdhelp_version"`
	Binary         string                     `json:"binary"`
	Version        string                     `json:"version,omitempty"`
	Summary        string                     `json:"summary,omitempty"`
	Homepage       string                     `json:"homepage,omitempty"`
	GlobalFlags    map[string]Flag            `json:"global_flags,omitempty"`
	Commands       map[string]Command         `json:"commands"`
	Schemas        map[string]json.RawMessage `json:"schemas,omitempty"`
	Context        *Context                   `json:"context,omitempty"`
}

// Context holds dynamic facts spliced in by CLIs with session state
// (spec §7). Populated by TASK-936; left nil for static emission.
type Context struct {
	Workspace string `json:"workspace,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Auth      string `json:"auth,omitempty"`
}

// Command describes a single command in the tree.
type Command struct {
	Summary     string              `json:"summary"`
	Description string              `json:"description,omitempty"`
	Args        []Arg               `json:"args,omitempty"`
	Flags       map[string]Flag     `json:"flags,omitempty"`
	Stdin       *Stdin              `json:"stdin,omitempty"`
	Stdout      *Stdout             `json:"stdout,omitempty"`
	ExitCodes   map[string]ExitCode `json:"exit_codes,omitempty"`
	Examples    []Example           `json:"examples,omitempty"`
	SeeAlso     []string            `json:"see_also,omitempty"`
	Since       string              `json:"since,omitempty"`
	Stability   string              `json:"stability,omitempty"`
}

// Arg is a positional argument.
type Arg struct {
	Name        string        `json:"name"`
	Type        string        `json:"type"`
	Required    bool          `json:"required,omitempty"`
	Description string        `json:"description,omitempty"`
	Default     interface{}   `json:"default,omitempty"`
	Format      string        `json:"format,omitempty"`
	Repeatable  bool          `json:"repeatable,omitempty"`
	Enum        []interface{} `json:"enum,omitempty"`
	EnumSource  string        `json:"enum_source,omitempty"`
}

// Flag is a flag (option) on a command.
type Flag struct {
	Type        string        `json:"type"`
	Required    bool          `json:"required,omitempty"`
	Description string        `json:"description,omitempty"`
	Default     interface{}   `json:"default,omitempty"`
	Format      string        `json:"format,omitempty"`
	Repeatable  bool          `json:"repeatable,omitempty"`
	Enum        []interface{} `json:"enum,omitempty"`
	EnumSource  string        `json:"enum_source,omitempty"`
	NegateFlag  string        `json:"negate_flag,omitempty"`
}

// Stdin describes whether the command accepts stdin.
type Stdin struct {
	Accepted bool   `json:"accepted"`
	Format   string `json:"format,omitempty"`
}

// Stdout describes the command's success output.
type Stdout struct {
	TextTemplate  string `json:"text_template,omitempty"`
	JSONSchemaRef string `json:"json_schema_ref,omitempty"`
}

// ExitCode is a string-or-object union (spec §5.2). When only Description
// is set, it marshals as a bare string ("terse" form). When any of When,
// Recovery, or MessageTemplate is set, it marshals as an object ("rich"
// form). The schema's `oneOf: [string, object]` accepts both shapes.
type ExitCode struct {
	// Description is the terse-form text. Mutually exclusive with the
	// rich-form fields below — set Description OR (When [+ Recovery +
	// MessageTemplate]), not both.
	Description string `json:"-"`

	When            string `json:"-"`
	Recovery        string `json:"-"`
	MessageTemplate string `json:"-"`
}

// MarshalJSON serializes the union per spec §5.2.
func (e ExitCode) MarshalJSON() ([]byte, error) {
	hasRich := e.When != "" || e.Recovery != "" || e.MessageTemplate != ""
	if e.Description != "" && !hasRich {
		return json.Marshal(e.Description)
	}
	type rich struct {
		When            string `json:"when,omitempty"`
		Recovery        string `json:"recovery,omitempty"`
		MessageTemplate string `json:"message_template,omitempty"`
	}
	return json.Marshal(rich{
		When:            e.When,
		Recovery:        e.Recovery,
		MessageTemplate: e.MessageTemplate,
	})
}

// Example is a canonical invocation. Same source feeds both --format json
// and --format md (spec §6); MD rendering wraps cmd in a fenced bash block
// and renders note as accompanying prose.
type Example struct {
	Cmd  string `json:"cmd"`
	Note string `json:"note,omitempty"`
}
