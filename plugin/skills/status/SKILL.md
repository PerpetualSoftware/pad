---
description: Project status at a glance — dashboard, active plans, attention items
allowed-tools: ["Bash"]
---

Run `pad project dashboard --format json`. Before presenting results, handle these two
failure modes:
- `pad` CLI missing (command not found): say so and point at https://getpad.dev/install.
  Don't retry or guess at alternate paths.
- No `.pad.toml` found in the directory tree: say this project isn't linked to a Pad
  workspace yet and suggest `/pad:onboard`.

Otherwise, present it conversationally: collection summaries, active plan progress,
attention items (stalled/overdue), and suggested next actions.
