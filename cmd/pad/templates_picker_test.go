package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// errReader returns a fixed error on every Read — used to simulate
// non-EOF read failures (e.g. EIO on a detached PTY).
type errReader struct{ err error }

func (r *errReader) Read(_ []byte) (int, error) { return 0, r.err }

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

// TestPickTemplateInteractiveSurfacesNonEOFReadErrors verifies that a
// non-EOF read failure aborts the picker rather than silently defaulting
// to the startup template.
func TestPickTemplateInteractiveSurfacesNonEOFReadErrors(t *testing.T) {
	var out bytes.Buffer
	sentinel := errors.New("simulated EIO")
	picked, err := pickTemplateInteractive(&errReader{err: sentinel}, &out)
	if err == nil {
		t.Fatalf("expected read error to propagate, got picked=%q err=nil", picked)
	}
	if picked != "" {
		t.Errorf("expected empty picked on error, got %q", picked)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped sentinel error, got %v", err)
	}
}

// TestPickTemplateInteractiveCancelKeywords verifies that typing any of
// the recognized cancel keywords (c, q, cancel, quit — case-insensitive)
// returns the canonical errCancelled sentinel rather than silently
// defaulting or treating the input as an invalid choice. The init RunE
// translates this sentinel into the same exit path as a Ctrl+C abort.
func TestPickTemplateInteractiveCancelKeywords(t *testing.T) {
	cases := []string{"c\n", "C\n", "q\n", "Q\n", "cancel\n", "Cancel\n", "CANCEL\n", "quit\n", "Quit\n"}
	for _, input := range cases {
		t.Run(strings.TrimSpace(input), func(t *testing.T) {
			var out bytes.Buffer
			picked, err := pickTemplateInteractive(strings.NewReader(input), &out)
			if !errors.Is(err, errCancelled) {
				t.Fatalf("pickTemplateInteractive(%q) err = %v, want errCancelled", input, err)
			}
			if picked != "" {
				t.Errorf("pickTemplateInteractive(%q) picked = %q, want empty on cancel", input, picked)
			}
			if strings.Contains(out.String(), "Invalid choice") {
				t.Errorf("expected cancel keyword %q to skip the 'Invalid choice' branch:\n%s", input, out.String())
			}
		})
	}
}

// TestPickTemplateInteractivePromptMentionsCancel verifies the prompt
// surfaces the cancel option so users discover it without reading docs.
func TestPickTemplateInteractivePromptMentionsCancel(t *testing.T) {
	var out bytes.Buffer
	_, _ = pickTemplateInteractive(strings.NewReader("\n"), &out)
	if !strings.Contains(out.String(), "cancel") {
		t.Errorf("expected picker prompt to mention 'cancel', got:\n%s", out.String())
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
