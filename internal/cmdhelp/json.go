package cmdhelp

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Options configure a Build/EmitJSON call. All fields are optional except
// Binary, which is used as the document's `binary` key.
type Options struct {
	// Binary is the CLI binary name as invoked on the command line
	// (e.g. "pad"). Defaults to root.Name() when empty.
	Binary string

	// Version is the implementation's own software version, independent
	// of cmdhelp's wire-format version. Free-form.
	Version string

	// Homepage is the project's canonical URL. Optional.
	Homepage string

	// MaxDepth caps the walk at N levels below `target`. Pass a negative
	// value (-1 is conventional) for unlimited depth. 0 emits only the
	// target itself; 1 emits the target plus its direct subcommands; N
	// emits N levels of subcommands.
	//
	// Note that Go's zero value (0) means "target only". Callers that
	// want the unlimited default MUST pass -1 (or any negative value)
	// explicitly — see cmd/pad/help_cmdhelp.go for the canonical wiring.
	MaxDepth int

	// Now overrides time.Now for the markdown emitter's YAML frontmatter
	// `generated:` field. Useful for snapshot tests that need a stable
	// timestamp. Leave nil to use the real wall clock (UTC).
	Now func() time.Time
}

// EmitJSON walks the command tree below `target`, builds a cmdhelp v0.1
// Document, and writes it to w as indented JSON terminated by a newline.
//
// `root` MUST be the root cobra command (used to derive global flags and
// command-path keys relative to the binary). `target` is where the user
// requested help; pass root for both when the user runs `<cmd> help`.
func EmitJSON(target, root *cobra.Command, opts Options, w io.Writer) error {
	doc := Build(target, root, opts)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// Build returns a cmdhelp Document without serializing it. Callers that
// need to inspect or further mutate the document (tests, dynamic enum
// resolvers in TASK-936) should use this and serialize themselves.
func Build(target, root *cobra.Command, opts Options) *Document {
	binary := opts.Binary
	if binary == "" {
		binary = root.Name()
	}

	doc := &Document{
		CmdhelpVersion: Version,
		Binary:         binary,
		Version:        opts.Version,
		Summary:        firstLine(root.Short),
		Homepage:       opts.Homepage,
		Commands:       map[string]Command{},
	}

	if globals := collectFlags(root.PersistentFlags()); len(globals) > 0 {
		doc.GlobalFlags = globals
	}

	walk(target, root, doc, 0, opts.MaxDepth)

	return doc
}

func walk(cur, root *cobra.Command, doc *Document, depth, maxDepth int) {
	if cur == nil || cur.Hidden {
		return
	}

	// Skip the help command itself — it documents the cmdhelp surface,
	// not a regular command, and cobra installs it on every root.
	if cur.Name() == "help" && cur.Parent() != nil && cur.Parent() == root {
		return
	}

	// The root binary itself is described by top-level fields (binary,
	// summary, version, etc.) — don't also emit it as a command entry.
	if cur != root {
		path := commandPath(cur, root)
		if path != "" {
			doc.Commands[path] = buildCommand(cur)
		}
	}

	// `depth` is how many levels below the initial target we currently are.
	// `maxDepth` is the number of additional levels of descendants to emit;
	// children of target sit at depth=1, grandchildren at depth=2, etc.
	// Per spec §4, --depth=0 means "subcommand list" (immediate children of
	// target), so we recurse while depth+1 <= maxDepth+1 — i.e. while
	// depth <= maxDepth. maxDepth < 0 disables the cap.
	if maxDepth >= 0 && depth > maxDepth {
		return
	}
	for _, sub := range cur.Commands() {
		walk(sub, root, doc, depth+1, maxDepth)
	}
}

func commandPath(cmd, root *cobra.Command) string {
	full := cmd.CommandPath()
	rootPath := root.CommandPath()
	trimmed := strings.TrimPrefix(full, rootPath)
	return strings.TrimSpace(trimmed)
}

func buildCommand(cmd *cobra.Command) Command {
	out := Command{
		Summary: firstLine(cmd.Short),
	}

	// Long-form description: only emit if it adds something beyond Short.
	if longTrimmed := strings.TrimSpace(cmd.Long); longTrimmed != "" && longTrimmed != cmd.Short && firstLine(longTrimmed) != cmd.Short {
		out.Description = longTrimmed
	}

	out.Args = parseArgs(cmd)
	if flags := collectFlags(cmd.LocalFlags()); len(flags) > 0 {
		out.Flags = flags
	}
	out.Examples = parseExamples(cmd.Example)

	return out
}

// argRE matches positional arg placeholders in a cobra Use string:
//
//	<name>      → required
//	[name]      → optional
//	<name>...   → required + repeatable (variadic)
//	[name]...   → optional + repeatable (variadic)
//	[a|b|c]     → optional alternation (cobra ValidArgs convention) — enum-typed
//	<a|b|c>     → required alternation — enum-typed
//
// Cobra-conventional placeholders ([flags], [options], [command]) and
// embedded flag-like fragments ([--status X]) are filtered downstream
// in parseArgs.
var argRE = regexp.MustCompile(`<([^<>]+)>(\.\.\.)?|\[([^\[\]]+)\](\.\.\.)?`)

func parseArgs(cmd *cobra.Command) []Arg {
	matches := argRE.FindAllStringSubmatch(cmd.Use, -1)
	args := make([]Arg, 0, len(matches))
	for _, m := range matches {
		var inner, ellipsis string
		var required bool
		switch {
		case m[1] != "":
			inner = m[1]
			ellipsis = m[2]
			required = true
		case m[3] != "":
			inner = m[3]
			ellipsis = m[4]
			required = false
		default:
			continue
		}
		inner = strings.TrimSpace(inner)
		if inner == "" {
			continue
		}

		// Filter cobra-conventional placeholders that are not real args.
		lower := strings.ToLower(inner)
		if !required && (lower == "flags" || lower == "options" || lower == "command") {
			continue
		}
		// Filter embedded flag-like fragments such as `[--status X]`.
		if strings.HasPrefix(inner, "-") {
			continue
		}

		// Alternation: <a|b|c> or [a|b|c] → enum-typed positional.
		if strings.Contains(inner, "|") {
			parts := strings.Split(inner, "|")
			values := make([]interface{}, 0, len(parts))
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" {
					values = append(values, p)
				}
			}
			if len(values) > 0 {
				args = append(args, Arg{
					Name:       "value", // synthesized; cobra Use doesn't carry a name here
					Type:       "enum",
					Required:   required,
					Enum:       values,
					Repeatable: ellipsis != "",
				})
				continue
			}
		}

		// Plain <name> / [name]. Reject anything that looks like prose
		// (spaces, punctuation that wouldn't be in an arg identifier).
		if !validArgName(inner) {
			continue
		}
		args = append(args, Arg{
			Name:       inner,
			Type:       "string",
			Required:   required,
			Repeatable: ellipsis != "",
		})
	}

	// If cobra's ValidArgs is set and we have at least one positional,
	// attach those values as the first arg's enum. ValidArgs is the
	// authoritative machine-readable form (Use is for humans), so this
	// covers the case where Use says `[shell]` but the allowed values
	// only live on the cobra struct.
	if len(cmd.ValidArgs) > 0 && len(args) > 0 && args[0].Type == "string" {
		values := make([]interface{}, len(cmd.ValidArgs))
		for i, v := range cmd.ValidArgs {
			// cobra ValidArgs entries can carry shell-completion descriptions
			// after a tab; strip them so the enum carries just the values.
			if tab := strings.IndexByte(v, '\t'); tab >= 0 {
				v = v[:tab]
			}
			values[i] = v
		}
		args[0].Type = "enum"
		args[0].Enum = values
	}

	if len(args) == 0 {
		return nil
	}
	return args
}

// validArgName returns true if s looks like an ordinary identifier-style
// arg name. Used to reject embedded flag fragments and prose that the
// regex would otherwise capture from idiosyncratic Use strings.
func validArgName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r == '-' || r == '_' || r == '.' || r == '/':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func collectFlags(fs *pflag.FlagSet) map[string]Flag {
	if fs == nil {
		return nil
	}
	out := map[string]Flag{}
	fs.VisitAll(func(f *pflag.Flag) {
		// Skip cobra's auto-installed --help flag and any explicitly
		// hidden flag. These are noise in machine-readable output.
		if f.Hidden || f.Name == "help" {
			return
		}
		out[f.Name] = buildFlag(f)
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildFlag(f *pflag.Flag) Flag {
	typ, repeatable := mapPflagType(f.Value.Type())
	flag := Flag{
		Type:        typ,
		Description: f.Usage,
		Repeatable:  repeatable,
	}
	if f.DefValue != "" && !isZeroDefault(f.DefValue, typ, repeatable) {
		flag.Default = f.DefValue
	}
	return flag
}

// isZeroDefault returns true when DefValue is the conventional zero for
// the flag's type. Suppressing zero defaults keeps the emitted document
// small and avoids encoding "" / "0" / "false" / "[]" everywhere.
func isZeroDefault(def, typ string, repeatable bool) bool {
	if repeatable && (def == "[]" || def == "" || def == "[" || def == "]") {
		return true
	}
	switch typ {
	case "string":
		return def == ""
	case "int", "float":
		return def == "0" || def == "0.0"
	case "bool":
		return def == "false"
	case "duration":
		return def == "0s" || def == "0"
	}
	return false
}

// mapPflagType translates pflag Value.Type() strings to the cmdhelp v0.1
// type vocabulary (spec §5.1) and reports whether the flag is repeatable.
//
// pflag exposes a wider type space than cmdhelp; we map related types
// down to the closed cmdhelp set. Slice/array variants become the scalar
// type with repeatable=true. Unknown/unmapped types fall back to "string"
// so emission never fails on an exotic flag.
func mapPflagType(t string) (cmdhelpType string, repeatable bool) {
	switch t {
	case "string":
		return "string", false
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"count":
		return "int", false
	case "float32", "float64":
		return "float", false
	case "bool":
		return "bool", false
	case "duration":
		return "duration", false
	case "stringSlice", "stringArray", "stringToString":
		return "string", true
	case "intSlice":
		return "int", true
	case "boolSlice":
		return "bool", true
	case "ip", "ipMask", "ipNet", "ipSlice":
		return "string", t == "ipSlice"
	case "bytesHex", "bytesBase64":
		return "string", false
	default:
		return "string", false
	}
}

// parseExamples turns cobra's Example field (a single multiline string)
// into individual Example entries. Each non-empty, non-comment line is
// taken as one runnable invocation; comment lines (`# ...`) are dropped.
//
// The convention in pad's existing commands is two-space-indented lines
// like:
//
//	pad item create task "Fix" --priority high
//
// We trim leading/trailing whitespace per line.
func parseExamples(example string) []Example {
	example = strings.TrimSpace(example)
	if example == "" {
		return nil
	}
	var examples []Example
	for _, line := range strings.Split(example, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		examples = append(examples, Example{Cmd: line})
	}
	if len(examples) == 0 {
		return nil
	}
	return examples
}

// firstLine returns the first line of s with surrounding whitespace
// trimmed. Useful for collapsing a multi-line Long string into a Summary.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
