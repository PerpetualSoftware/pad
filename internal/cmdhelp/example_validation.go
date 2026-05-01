package cmdhelp

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// ValidateExamples is the contractual mechanism by which cmdhelp v0.1
// prevents documentation drift (spec §6, §11 Q5). For every example in
// every command of `doc`:
//
//  1. Tokenize the `cmd` string with a shell-aware splitter.
//  2. Resolve the non-flag tokens against `root`'s live cobra command
//     tree. Failure means the example references a command path that
//     doesn't exist (typo, deleted command, drift).
//  3. Verify every `--flag` token resolves to a flag on the resolved
//     command — local, persistent, or inherited from any parent.
//
// Returns a slice of error strings, one per finding, with the offending
// command path + example index for fast triage. Empty slice means clean.
//
// This is intentionally a function not a test so cmd/pad's own test
// suite (which has access to the real cobra tree) can call it.
// Callers must pass `root` so flag-tree walks can include persistent
// flags from the binary root.
func ValidateExamples(doc *Document, root *cobra.Command) []string {
	if doc == nil || root == nil {
		return nil
	}
	var findings []string

	for path, cmd := range doc.Commands {
		for i, ex := range cmd.Examples {
			if ferrs := validateExample(path, i, ex, doc, root); len(ferrs) > 0 {
				findings = append(findings, ferrs...)
			}
		}
	}
	return findings
}

func validateExample(path string, idx int, ex Example, doc *Document, root *cobra.Command) []string {
	tokens, err := shellSplit(ex.Cmd)
	if err != nil {
		return []string{fmt.Sprintf("%s example[%d]: cannot tokenize %q: %v", path, idx, ex.Cmd, err)}
	}
	if len(tokens) == 0 {
		return []string{fmt.Sprintf("%s example[%d]: empty cmd string", path, idx)}
	}

	// Token 0 should be the binary's name. Examples that begin with
	// something else (e.g. `cat ~/foo.json | jq`) are documentation
	// snippets showing related output, not pad invocations the
	// validator can check. Skip them rather than fail — the drift
	// contract is for pad-invocation drift specifically.
	binary := root.Name()
	if tokens[0] != binary {
		return nil
	}

	// Pass the full post-binary token stream to cobra's Find. Cobra
	// knows each command's flag set and skips flag/value pairs while
	// matching subcommand names — so examples that interleave flags
	// with subcommands (e.g. `pad --workspace foo item create task`)
	// resolve to `item create`, not the root.
	target, _, ferr := root.Find(tokens[1:])
	if ferr != nil || target == nil {
		return []string{fmt.Sprintf("%s example[%d]: command path doesn't resolve: %s", path, idx, ex.Cmd)}
	}

	// Validate every --flag/-f against target's flag tree.
	// Note: flags can appear before the subcommand path on cobra
	// (e.g. `pad --workspace foo item create ...`), so scan all tokens.
	var findings []string
	skipNext := false
	for _, t := range tokens[1:] {
		if skipNext {
			skipNext = false
			continue
		}
		if !strings.HasPrefix(t, "-") {
			continue
		}
		// Strip leading dashes and any =value suffix.
		name := strings.TrimLeft(t, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if name == "" {
			continue // bare "--" terminator
		}
		// Walk target up to root, accept the flag if any level has it.
		// Boolean flags don't consume the next token; non-bool flags
		// do — but we only need the name check for drift detection,
		// so don't bother with the value-consumption walk except to
		// note that the "next token" might be a value rather than
		// another flag (no special handling needed here).
		if !flagExists(target, name) {
			// Try negate-flag form: --no-<rest>.
			if strings.HasPrefix(name, "no-") {
				stripped := strings.TrimPrefix(name, "no-")
				if flagExists(target, stripped) {
					continue
				}
			}
			findings = append(findings, fmt.Sprintf("%s example[%d]: unknown flag --%s in %q", path, idx, name, ex.Cmd))
		}
	}
	return findings
}

// flagExists reports whether `name` is a known flag on cmd or any of
// its ancestors (covers persistent / inherited flags). Both long and
// short forms are checked — pflag's Lookup treats single-character
// names as shorthand and ShorthandLookup as the lookup for them.
func flagExists(cmd *cobra.Command, name string) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Flags().Lookup(name) != nil {
			return true
		}
		if c.PersistentFlags().Lookup(name) != nil {
			return true
		}
		if len(name) == 1 {
			if c.Flags().ShorthandLookup(name) != nil {
				return true
			}
			if c.PersistentFlags().ShorthandLookup(name) != nil {
				return true
			}
		}
	}
	return false
}

// shellSplit tokenizes a command line in a POSIX-ish way: whitespace
// separates tokens, double-quotes group, single-quotes group (no
// expansion inside), backslash escapes the next rune outside single
// quotes. Stops at the first unquoted pipeline boundary (`|`, `;`,
// `&`, `>`, `<`) so the validator only checks the first command in a
// pipeline. Not a full shell — no $vars, no globbing.
//
// Sufficient for cmdhelp `examples[].cmd` strings, which are expected
// to be runnable invocations of one CLI command (possibly piped to
// another tool — that other tool is the user's shell, not pad's).
func shellSplit(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	var inDQuote, inSQuote, escape bool
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case escape:
			cur.WriteRune(r)
			escape = false
		case r == '\\' && !inSQuote:
			escape = true
		case r == '"' && !inSQuote:
			inDQuote = !inDQuote
		case r == '\'' && !inDQuote:
			inSQuote = !inSQuote
		case (r == ' ' || r == '\t') && !inDQuote && !inSQuote:
			flush()
		case (r == '|' || r == ';' || r == '&' || r == '>' || r == '<') && !inDQuote && !inSQuote:
			// Pipeline boundary — return what we have so far. The rest
			// of the string belongs to a different command (or the
			// shell), which is out of scope for cmdhelp validation.
			flush()
			return tokens, nil
		default:
			cur.WriteRune(r)
		}
	}
	if inDQuote || inSQuote {
		return nil, fmt.Errorf("unterminated quote")
	}
	if escape {
		return nil, fmt.Errorf("trailing backslash")
	}
	flush()
	return tokens, nil
}

// ValidateBoolArity asserts that every bool-typed flag in doc obeys
// spec §5.3: bool flags MUST be presence switches. They MUST NOT
// appear in `--flag=value` form anywhere in their command's examples
// (those should be declared as enum-typed instead).
//
// Returns one finding per violation. Empty slice means clean.
func ValidateBoolArity(doc *Document) []string {
	if doc == nil {
		return nil
	}
	var findings []string
	for path, cmd := range doc.Commands {
		for fname, f := range cmd.Flags {
			if f.Type != "bool" {
				continue
			}
			needle := "--" + fname + "="
			for i, ex := range cmd.Examples {
				if strings.Contains(ex.Cmd, needle) {
					findings = append(findings,
						fmt.Sprintf("%s example[%d]: bool flag --%s appears in valued form (--%s=...) — spec §5.3 requires bool flags to be presence-only; declare as enum: [\"true\",\"false\"] for valued booleans",
							path, i, fname, fname))
				}
			}
		}
	}
	return findings
}
