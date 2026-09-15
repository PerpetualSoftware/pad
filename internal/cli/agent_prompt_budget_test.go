package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// The compact dispatcher should stay small enough to load eagerly. Detailed
// reference material belongs behind `pad agent guide`, not in this payload.
const agentSkillPromptBudget = 5 * 1024

func TestAgentSkillPromptBudget(t *testing.T) {
	skill, err := os.ReadFile(filepath.Join("..", "..", "skills", "pad", "SKILL.md"))
	if err != nil {
		t.Fatalf("read embedded Pad skill: %v", err)
	}

	for _, agent := range []string{"cursor", "codex"} {
		t.Run(agent, func(t *testing.T) {
			tool := ResolveTool(agent)
			if tool == nil {
				t.Fatalf("ResolveTool(%q) returned nil", agent)
			}
			payload := FormatForTool(*tool, skill)
			t.Logf("%s installed skill payload: %d bytes (budget %d)", agent, len(payload), agentSkillPromptBudget)
			if len(payload) > agentSkillPromptBudget {
				t.Fatalf("%s installed skill payload is %d bytes; budget is %d", agent, len(payload), agentSkillPromptBudget)
			}
			for _, required := range []string{"pad bootstrap", "do not rerun bootstrap", "issue IDs", "convention_index", "pad agent guide"} {
				if !contains(string(payload), required) {
					t.Errorf("%s installed skill is missing core guidance %q", agent, required)
				}
			}
		})
	}
}
