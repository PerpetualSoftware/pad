---
description: Capture a thought as a Pad item without breaking flow
argument-hint: <the thing to capture>
allowed-tools: ["Bash"]
---

If the `pad` CLI is missing (command not found), say so and point at
https://getpad.dev/install — don't retry or guess at alternate paths. If no
`.pad.toml` is found in the directory tree, first run `pad auth whoami`
(fast, safe, never blocks): if it shows a real user, this machine is already
set up — run `pad workspace init` to link and create the workspace
non-interactively, then continue capturing. If it reports anything else (not
configured, session expired), this machine hasn't finished setup — don't run
`pad init`/`pad workspace init` yourself. Both now fail fast when
stdin/stdout aren't terminals instead of hanging (BUG-2538/BUG-2577/
BUG-2592) — though on a harness whose stdin AND stdout look like a real
terminal they still drop into the browser flow — and either way the outcome
just means a human needs an interactive terminal, which doesn't help you. Tell the user to run `pad init` themselves in an
interactive terminal (in Claude Code, suggest `! pad init`) and stop.

Before creating, run `pad item list conventions --field trigger=always
--field status=active --format json --full` and apply whatever it returns —
always-on conventions are project rules the workspace has made mandatory
(privacy, sizing, format, etc.), and low-ceremony capture means skipping the
full bootstrap weight (dashboard, playbooks, roles), not skipping mandatory
rules. This is the only context load this skill does; it's deliberately
narrower than the main skill's full bootstrap.

Capture `$ARGUMENTS` into the Pad workspace as the item type it most
resembles (idea, task, bug, note), following any conventions just loaded.
Run `pad collection list --format json` to see the workspace's collections,
pick the best fit, then show what you're about to create (collection, title,
content) and confirm before running `pad item create <collection> "<title>"
--content "..."`. Keep the title short; put nuance in the content.

**Exception:** if any loaded convention explicitly opts into autonomous
capture, skip the confirmation — create immediately and confirm with the new
issue ID in one line, no ceremony. Confirm-first is the default for everyone
else.
