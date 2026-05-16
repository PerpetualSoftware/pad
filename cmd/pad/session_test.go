package main

import (
	"strings"
	"testing"
)

// TestBuildSessionShape_ExplicitFlagErrors pins Codex R2 P3: a
// resolver error under an explicit --session must surface as an
// error, not as a silent fallback report. Typos in automation should
// fail loudly.
func TestBuildSessionShape_ExplicitFlagErrors(t *testing.T) {
	// Path-traversal-shaped session ID — resolver rejects shape, error
	// must propagate.
	_, err := buildSessionShape("../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path-traversal --session, got nil")
	}
	if !strings.Contains(err.Error(), "--session") {
		t.Errorf("error %q should mention --session for context", err.Error())
	}

	// Absolute-path that doesn't exist — also must error.
	_, err = buildSessionShape("/nonexistent/path/to/session.jsonl")
	if err == nil {
		t.Fatal("expected error for missing --session path, got nil")
	}
}

// TestBuildSessionShape_ImplicitFallback pins the inverse: when no
// --session was given and the resolver fails, we still return a
// fallback report (no error) so agents on non-Claude-Code harnesses
// can call the command without crashing.
func TestBuildSessionShape_ImplicitFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDECODE", "0")

	report, err := buildSessionShape("")
	if err != nil {
		t.Fatalf("implicit fallback should not error: %v", err)
	}
	if !report.FallbackUsed {
		t.Error("expected FallbackUsed=true")
	}
	if report.Agent != "unknown" {
		t.Errorf("Agent = %q, want unknown", report.Agent)
	}
}
