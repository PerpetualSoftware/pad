---
description: Capture a thought as a Pad item without breaking flow
argument-hint: <the thing to capture>
allowed-tools: ["Bash"]
---

Capture `$ARGUMENTS` into the Pad workspace as the item type it most resembles
(idea, task, bug, note). Run `pad collection list --format json` to see the
workspace's collections, pick the best fit, then
`pad item create <collection> "<title>" --content "..."`. Keep the title short;
put nuance in the content. Confirm with the new issue ID in one line — no ceremony.
