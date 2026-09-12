package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// This initially records the existing shared Cursor/Codex payload with modest
// headroom. Later prompt-reduction changes should lower this budget, not spend it.
const agentSkillPromptBudget = 40 * 1024

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
		})
	}
}
