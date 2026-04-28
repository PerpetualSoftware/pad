package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"golang.org/x/term"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

const defaultTemplateName = "startup"

// printGroupedTemplates prints the visible templates to w, grouped by
// category in canonical order. Used by `--list-templates` and the
// "unknown template" error path.
func printGroupedTemplates(w io.Writer) {
	bold := color.New(color.Bold)
	dim := color.New(color.Faint)
	groups := collections.GroupTemplatesByCategory()

	// Width of the template-name column, so descriptions line up.
	nameWidth := 0
	for _, g := range groups {
		for _, t := range g.Templates {
			if len(t.Name) > nameWidth {
				nameWidth = len(t.Name)
			}
		}
	}
	if nameWidth < 10 {
		nameWidth = 10
	}

	for i, g := range groups {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, bold.Sprint(collections.CategoryLabel(g.Category)))
		for _, t := range g.Templates {
			marker := ""
			if t.Name == defaultTemplateName {
				marker = " " + dim.Sprint("(default)")
			}
			icon := t.Icon
			if icon == "" {
				icon = " "
			}
			fmt.Fprintf(w, "  %s  %-*s  %s%s\n", icon, nameWidth, t.Name, t.Description, marker)
		}
	}
}

// canPromptForTemplate returns true when stdin and stdout are both TTYs —
// i.e. we can interactively prompt the user for a template choice.
func canPromptForTemplate() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// pickTemplateInteractive presents a numbered list of visible templates
// grouped by category and returns the chosen template's Name. Returns the
// default template name (startup) when the user presses enter without
// picking. Returns an error only on unrecoverable input failure.
//
// Callers should only invoke this when canPromptForTemplate() returns true.
func pickTemplateInteractive(in io.Reader, out io.Writer) (string, error) {
	bold := color.New(color.Bold)
	dim := color.New(color.Faint)

	groups := collections.GroupTemplatesByCategory()

	// Build a flat ordered list of templates while preserving group
	// boundaries so we can show headers inline with numbered choices.
	type numbered struct {
		index int
		tmpl  collections.WorkspaceTemplate
	}
	var flat []numbered

	fmt.Fprintln(out)
	fmt.Fprintln(out, bold.Sprint("Choose a workspace template:"))

	nameWidth := 0
	for _, g := range groups {
		for _, t := range g.Templates {
			if len(t.Name) > nameWidth {
				nameWidth = len(t.Name)
			}
		}
	}
	if nameWidth < 12 {
		nameWidth = 12
	}

	counter := 0
	for _, g := range groups {
		fmt.Fprintln(out)
		fmt.Fprintln(out, dim.Sprintf("  %s", collections.CategoryLabel(g.Category)))
		for _, t := range g.Templates {
			counter++
			flat = append(flat, numbered{index: counter, tmpl: t})
			marker := ""
			if t.Name == defaultTemplateName {
				marker = " " + dim.Sprint("(default)")
			}
			icon := t.Icon
			if icon == "" {
				icon = " "
			}
			fmt.Fprintf(out, "    %2d. %s  %-*s  %s%s\n", counter, icon, nameWidth, t.Name, t.Description, marker)
		}
	}

	// Resolve the default's display index for the prompt.
	defaultIdx := 1
	for _, n := range flat {
		if n.tmpl.Name == defaultTemplateName {
			defaultIdx = n.index
			break
		}
	}

	reader := bufio.NewReader(in)
	for {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Select a template [%d] (press enter for default, 'c' to cancel): ", defaultIdx)
		line, err := reader.ReadString('\n')
		if err != nil {
			// EOF is a benign signal (pipe closed, stdin exhausted in a
			// test) and should fall back to the default template. Other
			// read errors (e.g. EIO from a detached PTY) must abort so
			// we don't silently apply a template the user never chose.
			if errors.Is(err, io.EOF) {
				return defaultTemplateName, nil
			}
			return "", fmt.Errorf("read template selection: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return defaultTemplateName, nil
		}
		// Explicit cancel — equivalent to Ctrl+C. Returning errCancelled
		// lets the caller treat this exactly like a SIGINT abort.
		switch strings.ToLower(line) {
		case "c", "q", "cancel", "quit":
			return "", errCancelled
		}
		// Allow typing a name directly as an escape hatch.
		if tmpl := collections.GetTemplate(line); tmpl != nil && !tmpl.Hidden {
			return tmpl.Name, nil
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(flat) {
			fmt.Fprintf(out, "Invalid choice %q. Enter a number between 1 and %d, a template name, or 'c' to cancel.\n", line, len(flat))
			continue
		}
		return flat[n-1].tmpl.Name, nil
	}
}
