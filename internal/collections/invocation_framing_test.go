package collections

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestInvocationFramingStaysNLCanonical is the drift-guard for PLAN-1858 /
// IDEA-1846: natural language is the canonical way to invoke a playbook, and
// `/pad <slug>` · `$pad <slug>` · the pad_playbook MCP form are per-surface
// shortcuts. This test fails when agent-facing copy reintroduces a phrasing
// that frames `/pad <slug>` as THE invocation form.
//
// It is a deliberate BACKSTOP, not a generator: it denies the known
// canonical-framing phrasings ("maps directly to /pad", "invoke via /pad",
// "say /pad …", etc.) rather than scanning every `/pad` occurrence — that
// keeps it low-false-positive, since legitimate labeled shortcuts
// ("/pad ship in Claude Code") and the slug-routing examples are fine. The
// companion convention (seeded via this plan) covers the spirit for surfaces
// this test doesn't mechanically scan (the MCP catalog/prompt Go consts, the
// web UI).
//
// Scope: the two plain-text agent-instruction files (SKILL.md, the MCP
// server instructions) plus the rendered seeded playbook bodies. If you're
// adding a new seeded playbook, add its body to the surfaces map below.
func TestInvocationFramingStaysNLCanonical(t *testing.T) {
	root := repoRoot(t)

	readFile := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}

	// name -> content. Plain-text files are read from disk; seeded bodies are
	// the rendered package consts (no Go-source escaping to fight).
	surfaces := map[string]string{
		"skills/pad/SKILL.md":                readFile("skills/pad/SKILL.md"),
		"internal/mcp/instructions.md":       readFile("internal/mcp/instructions.md"),
		"playbook_library_plan.go body":      planPlaybookBody,
		"playbook_library_decompose.go body": decomposePlaybookBody,
		"playbook_library_onboard.go body":   onboardPlaybookBody,
		"templates_startup_ship.go body":     shipPlaybookBody,
	}

	// Canonical-framing phrasings to reject. Each matches "<framing verb> …
	// /pad" — i.e. copy that presents the slash shortcut as the way in. The
	// optional backtick/asterisks tolerate markdown emphasis.
	banned := []*regexp.Regexp{
		regexp.MustCompile("(?i)maps directly to\\s+[`*]*/pad"),
		regexp.MustCompile("(?i)invoke via\\s+[`*]*/pad"),
		regexp.MustCompile("(?i)\\bsay\\s+[`*]*/pad"),
		regexp.MustCompile("(?i)directly invokable as\\s+[`*]*/pad"),
		regexp.MustCompile("(?i)canonical\\s+[`*]*/pad"),
		regexp.MustCompile("(?i)dispatches\\s+[`*]*/pad"),
		regexp.MustCompile("(?i)playbooks available:\\s*[`*]*/pad"),
	}

	// Self-check: each banned pattern must actually match its representative
	// canonical-framing phrasing. Guards against a typo silently neutering a
	// regex (which would let the main scan pass vacuously).
	badExamples := []string{
		"that maps directly to `/pad <slug>` in chat",
		"Invoke via /pad ship",
		"say `/pad onboard` to walk through setup",
		"directly invokable as `/pad <slug>`",
		"the canonical `/pad onboard` invokable playbook",
		"the agent dispatches `/pad <slug>` directly",
		"Playbooks available: `/pad ship`, `/pad release`",
	}
	for i, re := range banned {
		if !re.MatchString(badExamples[i]) {
			t.Fatalf("banned pattern %q failed to match its own example %q — the regex is broken", re.String(), badExamples[i])
		}
	}

	for name, content := range surfaces {
		for i, line := range strings.Split(content, "\n") {
			for _, re := range banned {
				if re.MatchString(line) {
					t.Errorf("%s:%d frames `/pad` as canonical (matched %q) — lead with natural language and label the slash form as a per-surface shortcut.\n  line: %s",
						name, i+1, re.String(), strings.TrimSpace(line))
				}
			}
		}
	}

	// Positive anchor: the principle must stay stated in the skill. If this
	// sentence is deleted/reworded the guard above could pass vacuously.
	skill := surfaces["skills/pad/SKILL.md"]
	if !strings.Contains(strings.ToLower(skill), "natural language is the canonical way to invoke a playbook") {
		t.Errorf("SKILL.md no longer states the NL-canonical invocation principle — the drift-guard relies on it being documented")
	}
}

// repoRoot walks up from this test file to the directory containing go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (go.mod)")
		}
		dir = parent
	}
}
