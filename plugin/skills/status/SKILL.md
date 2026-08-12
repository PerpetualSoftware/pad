---
description: Project status at a glance — dashboard, active plans, attention items
allowed-tools: ["Bash"]
---

Run `pad project dashboard --format json` and present it conversationally: collection
summaries, active plan progress, attention items (stalled/overdue), and suggested next
actions. If the `pad` CLI is missing, say so and point at https://getpad.dev/install.
If no `.pad.toml` is found in the directory tree, say this project isn't linked to a
Pad workspace yet and suggest `/pad:onboard`.
