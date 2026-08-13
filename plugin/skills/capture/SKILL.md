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
`pad init`/`pad workspace init` yourself (verified live: it can hang waiting
on a browser-based setup flow); tell the user to run `pad init` themselves in
an interactive terminal (in Claude Code, suggest `! pad init`) and stop.

Capture `$ARGUMENTS` into the Pad workspace as the item type it most resembles
(idea, task, bug, note). Run `pad collection list --format json` to see the
workspace's collections, pick the best fit, then show what you're about to
create (collection, title, content) and confirm before running
`pad item create <collection> "<title>" --content "..."`. Keep the title
short; put nuance in the content.

**Exception:** if the workspace's active conventions explicitly opt into
autonomous capture (check `pad item list conventions --field trigger=always
--field status=active` for a rule that says so), skip the confirmation —
create immediately and confirm with the new issue ID in one line, no
ceremony. Confirm-first is the default for everyone else.
