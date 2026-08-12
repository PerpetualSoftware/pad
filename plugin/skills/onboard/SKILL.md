---
description: Link this project to a Pad workspace and set it up
allowed-tools: ["Bash", "Read"]
---

Set this project up with Pad. If `pad` is not on PATH, give the install one-liner
from https://getpad.dev/install and stop. If no `.pad.toml` exists, run
`pad workspace init` (interactive; surface its prompts). Then run
`pad bootstrap --format json`: if `needs_onboarding` is true, offer the workspace
onboarding flow — scan the codebase (README, build config, CI), suggest starter
conventions with the project's real commands, and propose an initial plan. Create
items only after the user confirms the shape.
