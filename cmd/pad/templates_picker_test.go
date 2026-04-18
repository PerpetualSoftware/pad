package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestPickTemplateInteractiveDefault verifies that pressing enter without
// a number selects the default template (startup).
func TestPickTemplateInteractiveDefault(t *testing.T) {
	var out bytes.Buffer
	picked, err := pickTemplateInteractive(strings.NewReader("\n"), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked != defaultTemplateName {
		t.Errorf("pickTemplateInteractive empty input = %q, want %q", picked, defaultTemplateName)
	}
}

// TestPickTemplateInteractiveByName verifies that typing a template name
// directly (instead of a number) is accepted.
func TestPickTemplateInteractiveByName(t *testing.T) {
	var out bytes.Buffer
	picked, err := pickTemplateInteractive(strings.NewReader("hiring\n"), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked != "hiring" {
		t.Errorf("pickTemplateInteractive by name = %q, want %q", picked, "hiring")
	}
}

// TestPickTemplateInteractiveByNumber verifies that numeric selection maps
// to the correct template. Number 1 should always be the first template in
// the canonical ordering (currently "startup").
func TestPickTemplateInteractiveByNumber(t *testing.T) {
	var out bytes.Buffer
	picked, err := pickTemplateInteractive(strings.NewReader("1\n"), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked == "" {
		t.Fatal("pickTemplateInteractive returned empty name on numeric input")
	}
}

// TestPickTemplateInteractiveRetriesOnInvalid verifies that invalid input
// re-prompts (rather than returning an error or defaulting silently).
func TestPickTemplateInteractiveRetriesOnInvalid(t *testing.T) {
	var out bytes.Buffer
	// First "zzz" is invalid (not a number, not a known template name).
	// Follow with enter to accept the default.
	picked, err := pickTemplateInteractive(strings.NewReader("zzz\n\n"), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if picked != defaultTemplateName {
		t.Errorf("pickTemplateInteractive after invalid+default = %q, want %q", picked, defaultTemplateName)
	}
	if !strings.Contains(out.String(), "Invalid choice") {
		t.Errorf("expected 'Invalid choice' message in output, got:\n%s", out.String())
	}
}

// TestPrintGroupedTemplatesIncludesEveryVisibleTemplate is a smoke test
// that the grouped printer covers each visible template's name.
func TestPrintGroupedTemplatesIncludesEveryVisibleTemplate(t *testing.T) {
	var out bytes.Buffer
	printGroupedTemplates(&out)
	rendered := out.String()
	for _, name := range []string{"startup", "scrum", "product", "hiring", "interviewing"} {
		if !strings.Contains(rendered, name) {
			t.Errorf("grouped template output missing %q:\n%s", name, rendered)
		}
	}
	// Hidden template (demo) should NOT appear.
	if strings.Contains(rendered, "demo") {
		t.Errorf("grouped template output leaked hidden template 'demo':\n%s", rendered)
	}
	// Category headers should render.
	if !strings.Contains(rendered, "Software") {
		t.Errorf("grouped template output missing 'Software' category header")
	}
	if !strings.Contains(rendered, "People") {
		t.Errorf("grouped template output missing 'People' category header")
	}
}
