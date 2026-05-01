package cmdhelp

import "strings"

// Canonical enum_source identifiers for the dynamic facts pad's CLI
// can splice into help output. Format: "dynamic:<command>" per spec §7.
//
// New tools adding cmdhelp may declare their own dynamic sources;
// these constants are pad-flavored and live here so the cli layer and
// tests reference them via stable names.
const (
	EnumSourceCollections = "dynamic:pad collection list"
	EnumSourceRoles       = "dynamic:pad role list"
	EnumSourceMembers     = "dynamic:pad workspace members"
)

// DynamicEnum resolves a single enum_source to its current values.
// Implementations close over a CLI client; see cmd/pad/help_cmdhelp.go
// for pad's wiring.
//
// Returning (nil, error) is treated as "no values available" — the
// document still announces enum_source so consumers know the binding,
// but Enum is left empty. This is the right behavior when the server
// is unreachable or auth is missing: the help command MUST NOT fail
// just because dynamic facts can't be fetched.
type DynamicEnum func() ([]interface{}, error)

// Resolver maps known arg/flag names to dynamic enum sources, plus the
// resolver functions that fetch live values for each source. It's
// passed via Options.Resolver and applied by Build after the static
// document is constructed.
//
// All maps use lowercase keys; lookups are case-insensitive.
type Resolver struct {
	// Workspace, when non-empty, populates doc.Context.Workspace so
	// markdown's `## Workspace context` section has something to render.
	Workspace string

	// ArgEnumSources binds positional arg names to enum_source.
	// Example: {"collection": EnumSourceCollections}.
	ArgEnumSources map[string]string

	// FlagEnumSources binds flag names to enum_source.
	// Example: {"role": EnumSourceRoles, "assign": EnumSourceMembers}.
	FlagEnumSources map[string]string

	// Sources maps enum_source string → resolver function. Functions
	// are called at most once per Apply call (results cached internally).
	Sources map[string]DynamicEnum
}

// Apply walks doc and stamps EnumSource + resolved Enum values on
// matching args and flags. Resolution failures are silent: the document
// continues to advertise enum_source even when no values are available,
// so consumers can fall back to invoking the dynamic command themselves.
//
// Apply is a no-op when r is nil — callers may pass nil from
// Options.Resolver to disable dynamic resolution entirely.
func (r *Resolver) Apply(doc *Document) {
	if r == nil || doc == nil {
		return
	}

	// Per-Apply cache: each enum_source resolved at most once even when
	// many commands reference it (e.g. every `item` subcommand has a
	// `collection` arg).
	cache := make(map[string][]interface{}, len(r.Sources))
	resolve := func(src string) []interface{} {
		if cached, ok := cache[src]; ok {
			return cached
		}
		fn, ok := r.Sources[src]
		if !ok {
			cache[src] = nil
			return nil
		}
		values, err := fn()
		if err != nil {
			values = nil
		}
		cache[src] = values
		return values
	}

	// Global flags first — same lookup rules as per-command flags.
	for name, f := range doc.GlobalFlags {
		src, ok := r.FlagEnumSources[strings.ToLower(name)]
		if !ok {
			continue
		}
		f.EnumSource = src
		values := resolve(src)
		if len(values) > 0 && len(f.Enum) == 0 {
			f.Enum = values
			if f.Type == "string" {
				f.Type = "enum"
			}
		}
		doc.GlobalFlags[name] = f
	}

	for path, cmd := range doc.Commands {
		// Args: in-place via index since cmd.Args is a slice value
		// inside the map's struct value.
		for i := range cmd.Args {
			src, ok := r.ArgEnumSources[strings.ToLower(cmd.Args[i].Name)]
			if !ok {
				continue
			}
			cmd.Args[i].EnumSource = src
			values := resolve(src)
			// Don't replace existing Enum values from alternation /
			// ValidArgs — those are the authoritative spec for that arg
			// and dynamic resolution is only meant to fill the gap.
			if len(values) > 0 && len(cmd.Args[i].Enum) == 0 {
				cmd.Args[i].Enum = values
				// Upgrade type from generic string → enum, but preserve
				// any non-string type that was set deliberately upstream.
				if cmd.Args[i].Type == "string" {
					cmd.Args[i].Type = "enum"
				}
			}
		}
		// Flags: map values must be re-stored after mutation (Go map
		// values are not addressable).
		for name, f := range cmd.Flags {
			src, ok := r.FlagEnumSources[strings.ToLower(name)]
			if !ok {
				continue
			}
			f.EnumSource = src
			values := resolve(src)
			if len(values) > 0 && len(f.Enum) == 0 {
				f.Enum = values
				if f.Type == "string" {
					f.Type = "enum"
				}
			}
			cmd.Flags[name] = f
		}
		doc.Commands[path] = cmd
	}

	// Workspace context for markdown's ## Workspace context section.
	if r.Workspace != "" {
		if doc.Context == nil {
			doc.Context = &Context{}
		}
		doc.Context.Workspace = r.Workspace
	}
}
