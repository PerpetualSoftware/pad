# Pad — Claude Code Skill

## Installation

### 1. Install the Pad binary

```bash
# Via Homebrew
brew install PerpetualSoftware/tap/pad

# Or build from source
git clone https://github.com/PerpetualSoftware/pad
cd pad && make build
cp pad /usr/local/bin/
```

### 2. Initialize a workspace and install the skill

```bash
cd ~/projects/myapp
pad workspace init "My App"
```

`pad workspace init` will detect that the `/pad` skill isn't installed and offer to install it for detected tools. You can also install manually:

```bash
pad agent install claude     # Install for Claude Code
pad agent status             # Check installed integrations
pad agent update             # Update the version bundled in the binary
```

When you update the `pad` binary, run `pad agent status` to check if your installed integrations are outdated. If they are, run `pad agent update` to sync them.

### 3. Use it

Type `/pad` in Claude Code followed by anything in natural language:

```
/pad what should I work on next?
/pad I finished the OAuth fix
/pad create a task to add rate limiting --priority high
/pad let's brainstorm about the API redesign
/pad how far along are we?
/pad prep for standup
```

The `/pad` skill is a natural-language interface — there are no rigid commands. It interprets your intent and uses the `pad` CLI under the hood.

## How It Works

1. The `/pad` skill is a single `SKILL.md` file installed at `.claude/skills/pad/SKILL.md`
2. It tells Claude how to use the `pad` CLI to manage your project
3. The CLI auto-starts a local server on first use — no setup needed
4. Data is stored in SQLite at `~/.pad/pad.db`
5. The web UI is at `http://localhost:7777` (embedded in the binary)
6. All agent actions are attributed as "agent via skill" in the activity feed
7. The agent loads and follows your project conventions and playbooks automatically

## Web UI

```bash
pad server open
# or visit http://localhost:7777
```
