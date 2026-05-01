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
//
// Two binding scopes are supported:
//
//  1. **Wildcard** (ArgEnumSources / FlagEnumSources) — applied to
//     every command. Use only when the arg/flag name has a unique
//     semantic across the entire CLI (e.g. an `<collection>` arg
//     always refers to a pad collection regardless of which command
//     declares it).
//
//  2. **Per-command** (CommandArgBindings / CommandFlagBindings) —
//     scoped to a specific command path. Use whenever the same name
//     can mean different things in different places — for example
//     `--role` accepts agent-role slugs on item commands but accepts
//     workspace roles (owner / editor / viewer) on `workspace invite`.
//
// Per-command bindings win over wildcards when both match.
type Resolver struct {
	// Workspace, when non-empty, populates doc.Context.Workspace so
	// markdown's `## Workspace context` section has something to render.
	Workspace string

	// ArgEnumSources binds positional arg names to enum_source globally.
	// Example: {"collection": EnumSourceCollections}.
	ArgEnumSources map[string]string

	// FlagEnumSources binds flag names to enum_source globally.
	// Use cautiously: a single name often means different things on
	// different commands. Prefer CommandFlagBindings unless you've
	// verified the name is unambiguous across the entire CLI.
	FlagEnumSources map[string]string

	// CommandArgBindings: per-command-path overrides for positional args.
	// Outer key is the command path (e.g. "item create"); inner key is
	// arg name. Wins over ArgEnumSources when both match.
	CommandArgBindings map[string]map[string]string

	// CommandFlagBindings: per-command-path overrides for flags.
	// Outer key is the command path (e.g. "item create"); inner key is
	// flag name. Wins over FlagEnumSources when both match.
	CommandFlagBindings map[string]map[string]string

	// Sources maps enum_source string → resolver function. Functions
	// are called at most once per Apply call (results cached internally).
	Sources map[string]DynamicEnum
}

// argSource looks up the enum_source for a positional arg on the
// given command path. Returns ("", false) when no binding applies.
func (r *Resolver) argSource(path, name string) (string, bool) {
	name = strings.ToLower(name)
	if specific, ok := r.CommandArgBindings[path][name]; ok {
		return specific, true
	}
	if global, ok := r.ArgEnumSources[name]; ok {
		return global, true
	}
	return "", false
}

// flagSource looks up the enum_source for a flag on the given command
// path. Returns ("", false) when no binding applies.
func (r *Resolver) flagSource(path, name string) (string, bool) {
	name = strings.ToLower(name)
	if specific, ok := r.CommandFlagBindings[path][name]; ok {
		return specific, true
	}
	if global, ok := r.FlagEnumSources[name]; ok {
		return global, true
	}
	return "", false
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

	// Global flags: only the wildcard FlagEnumSources applies — there's
	// no command path to scope a per-command binding against.
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
			src, ok := r.argSource(path, cmd.Args[i].Name)
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
			src, ok := r.flagSource(path, name)
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
