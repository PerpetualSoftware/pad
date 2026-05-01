package cmdhelp

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// EmitMarkdown walks the command tree below `target`, builds a cmdhelp
// v0.1 Document, and writes it to w as a markdown document with the
// predictable section order from spec §6.
//
// `--format llm` is an alias for `--format md` at the routing layer; both
// reach this function. Callers that need to vary llm-specific behavior
// in the future can branch upstream — the markdown shape itself is the
// same regardless of which alias was invoked.
func EmitMarkdown(target, root *cobra.Command, opts Options, w io.Writer) error {
	doc := Build(target, root, opts)
	return RenderMarkdown(doc, opts, w)
}

// RenderMarkdown writes a Document as markdown matching cmdhelp v0.1 §6.
// Useful for tests that build a Document manually and want to render it
// without re-walking a cobra tree.
func RenderMarkdown(doc *Document, opts Options, w io.Writer) error {
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	// YAML frontmatter (spec §6).
	if _, err := fmt.Fprintln(w, "---"); err != nil {
		return err
	}
	fmt.Fprintf(w, "cmdhelp_version: %q\n", doc.CmdhelpVersion)
	fmt.Fprintf(w, "binary: %s\n", doc.Binary)
	if doc.Version != "" {
		fmt.Fprintf(w, "version: %s\n", doc.Version)
	}
	fmt.Fprintf(w, "generated: %s\n", now().UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "---")
	fmt.Fprintln(w)

	// Top-level binary heading + summary.
	fmt.Fprintf(w, "# %s\n\n", doc.Binary)
	if doc.Summary != "" {
		fmt.Fprintf(w, "%s\n\n", doc.Summary)
	}
	if doc.Homepage != "" {
		fmt.Fprintf(w, "Homepage: <%s>\n\n", doc.Homepage)
	}

	// Workspace context (dynamic, optional — populated in TASK-936).
	if ctx := doc.Context; ctx != nil && (ctx.Workspace != "" || ctx.Profile != "" || ctx.Auth != "") {
		fmt.Fprintln(w, "## Workspace context")
		fmt.Fprintln(w)
		if ctx.Workspace != "" {
			fmt.Fprintf(w, "- workspace: `%s`\n", ctx.Workspace)
		}
		if ctx.Profile != "" {
			fmt.Fprintf(w, "- profile: `%s`\n", ctx.Profile)
		}
		if ctx.Auth != "" {
			fmt.Fprintf(w, "- auth: `%s`\n", ctx.Auth)
		}
		fmt.Fprintln(w)
	}

	// Global flags (emitted once at top level — never duplicated per command).
	if len(doc.GlobalFlags) > 0 {
		fmt.Fprintln(w, "## Global flags")
		fmt.Fprintln(w)
		writeFlagsTable(w, doc.GlobalFlags)
		fmt.Fprintln(w)
	}

	// Commands. Sorted by path for deterministic output (golden snapshots).
	if len(doc.Commands) > 0 {
		paths := make([]string, 0, len(doc.Commands))
		for p := range doc.Commands {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			renderCommand(w, doc.Binary, p, doc.Commands[p])
		}
	}
	return nil
}

// renderCommand writes one command's markdown section. Section order
// matches cmdhelp v0.1 §6 exactly.
func renderCommand(w io.Writer, binary, path string, cmd Command) {
	fmt.Fprintf(w, "## `%s %s`\n\n", binary, path)

	if cmd.Summary != "" {
		fmt.Fprintf(w, "%s\n\n", cmd.Summary)
	}
	// Long-form description: only emit when it adds something beyond Summary.
	if cmd.Description != "" && cmd.Description != cmd.Summary {
		fmt.Fprintf(w, "%s\n\n", cmd.Description)
	}

	// 1. Synopsis
	fmt.Fprintln(w, "### Synopsis")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "```\n%s\n```\n\n", renderSynopsis(binary, path, cmd))

	// 2. Arguments
	if len(cmd.Args) > 0 {
		fmt.Fprintln(w, "### Arguments")
		fmt.Fprintln(w)
		writeArgsTable(w, cmd.Args)
		fmt.Fprintln(w)
	}

	// 3. Flags
	if len(cmd.Flags) > 0 {
		fmt.Fprintln(w, "### Flags")
		fmt.Fprintln(w)
		writeFlagsTable(w, cmd.Flags)
		fmt.Fprintln(w)
	}

	// 4. Stdin
	if cmd.Stdin != nil && cmd.Stdin.Accepted {
		fmt.Fprintln(w, "### Stdin")
		fmt.Fprintln(w)
		if cmd.Stdin.Format != "" {
			fmt.Fprintf(w, "Accepts stdin (`%s`).\n\n", cmd.Stdin.Format)
		} else {
			fmt.Fprintln(w, "Accepts stdin.")
			fmt.Fprintln(w)
		}
	}

	// 5. Examples — same canonical example set as JSON, rendered as
	//    fenced bash blocks with note as accompanying prose (spec §6).
	if len(cmd.Examples) > 0 {
		fmt.Fprintln(w, "### Examples")
		fmt.Fprintln(w)
		for _, ex := range cmd.Examples {
			fmt.Fprintf(w, "```bash\n%s\n```\n\n", ex.Cmd)
			if ex.Note != "" {
				fmt.Fprintf(w, "%s\n\n", ex.Note)
			}
		}
	}

	// 6. Output
	if cmd.Stdout != nil && (cmd.Stdout.TextTemplate != "" || cmd.Stdout.JSONSchemaRef != "") {
		fmt.Fprintln(w, "### Output")
		fmt.Fprintln(w)
		if cmd.Stdout.TextTemplate != "" {
			fmt.Fprintf(w, "Stdout (text): `%s`\n\n", cmd.Stdout.TextTemplate)
		}
		if cmd.Stdout.JSONSchemaRef != "" {
			fmt.Fprintf(w, "Stdout (`--format json`): schema `%s`\n\n", cmd.Stdout.JSONSchemaRef)
		}
	}

	// 7. Exit codes (when present — pad commands don't typically populate
	//    this yet, but the schema and spec §5.2 support it).
	if len(cmd.ExitCodes) > 0 {
		fmt.Fprintln(w, "### Exit codes")
		fmt.Fprintln(w)
		writeExitCodesTable(w, cmd.ExitCodes)
		fmt.Fprintln(w)
	}

	// 8. See also
	if len(cmd.SeeAlso) > 0 {
		fmt.Fprintln(w, "### See also")
		fmt.Fprintln(w)
		for _, related := range cmd.SeeAlso {
			fmt.Fprintf(w, "- `%s`\n", related)
		}
		fmt.Fprintln(w)
	}
}

// renderSynopsis reconstructs a cobra-style usage line from the
// Document's structured args + flags. Output looks like:
//
//	pad item create <collection> <title> [--priority <value>] [flags]
//
// We use the structured Args (not cmd.UseLine()) so the synopsis is
// driven by the same parsed data the JSON emitter uses — keeping the
// two formats in lockstep.
func renderSynopsis(binary, path string, cmd Command) string {
	parts := []string{binary}
	if path != "" {
		parts = append(parts, path)
	}
	for _, a := range cmd.Args {
		var token string
		if a.Required {
			token = "<" + a.Name + ">"
		} else {
			token = "[" + a.Name + "]"
		}
		if a.Repeatable {
			token += "..."
		}
		parts = append(parts, token)
	}
	if len(cmd.Flags) > 0 {
		parts = append(parts, "[flags]")
	}
	return strings.Join(parts, " ")
}

// writeFlagsTable renders a flag map as a markdown table with sorted
// keys. Repeatable flags are marked in the type column for visibility.
func writeFlagsTable(w io.Writer, flags map[string]Flag) {
	fmt.Fprintln(w, "| flag | type | default | description |")
	fmt.Fprintln(w, "| --- | --- | --- | --- |")
	names := make([]string, 0, len(flags))
	for n := range flags {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := flags[n]
		typ := f.Type
		if f.Repeatable {
			typ += " (repeatable)"
		}
		if len(f.Enum) > 0 {
			vals := make([]string, len(f.Enum))
			for i, v := range f.Enum {
				vals[i] = fmt.Sprint(v)
			}
			typ = fmt.Sprintf("enum: %s", strings.Join(vals, "\\|"))
		}
		def := ""
		if f.Default != nil {
			def = fmt.Sprintf("`%v`", f.Default)
		}
		fmt.Fprintf(w, "| `--%s` | %s | %s | %s |\n",
			n, typ, def, escapeTable(f.Description))
	}
}

// writeArgsTable renders positional args. Args are emitted in source
// order (no sorting) so the table reflects invocation order.
func writeArgsTable(w io.Writer, args []Arg) {
	fmt.Fprintln(w, "| name | type | required | description |")
	fmt.Fprintln(w, "| --- | --- | --- | --- |")
	for _, a := range args {
		typ := a.Type
		if a.Repeatable {
			typ += " (repeatable)"
		}
		if len(a.Enum) > 0 {
			vals := make([]string, len(a.Enum))
			for i, v := range a.Enum {
				vals[i] = fmt.Sprint(v)
			}
			typ = fmt.Sprintf("enum: %s", strings.Join(vals, "\\|"))
		}
		req := "no"
		if a.Required {
			req = "yes"
		}
		fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n",
			a.Name, typ, req, escapeTable(a.Description))
	}
}

// writeExitCodesTable renders an exit_codes map. Keys are numeric strings
// from the schema; sort them numerically (best-effort string sort works
// for sensible exit codes 0-255).
func writeExitCodesTable(w io.Writer, codes map[string]ExitCode) {
	fmt.Fprintln(w, "| code | when | recovery |")
	fmt.Fprintln(w, "| --- | --- | --- |")
	keys := make([]string, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := codes[k]
		when := c.When
		if when == "" {
			when = c.Description
		}
		fmt.Fprintf(w, "| `%s` | %s | %s |\n", k, escapeTable(when), escapeTable(c.Recovery))
	}
}

// escapeTable replaces newlines and pipes so a value can sit in a
// markdown table cell without breaking the table grid.
func escapeTable(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.TrimSpace(s)
}
