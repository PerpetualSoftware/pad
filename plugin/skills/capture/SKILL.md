---
description: Capture a thought as a Pad item without breaking flow
argument-hint: <the thing to capture>
allowed-tools: ["Bash"]
---

If the `pad` CLI is missing (command not found), say so and point at
https://getpad.dev/install — don't retry or guess at alternate paths.

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
