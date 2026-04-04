-- Add tools text field to agent_roles for free-text notes about preferred tools/models.
-- e.g. "Claude Code + Sonnet 4.6", "Codex CLI", "Cursor with GPT-4"
ALTER TABLE agent_roles ADD COLUMN tools TEXT NOT NULL DEFAULT '';
