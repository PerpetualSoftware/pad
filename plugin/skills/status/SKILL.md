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

If the response's `needs_onboarding` field is true, lead with the same active offer
the main skill uses — before anything else: "This workspace is brand new and isn't
set up yet. Want me to set it up? I'll ask a few quick questions and adapt it to your
project." This is an offer, not an auto-run; mention `/pad:onboard` as the shortcut
for later.

Otherwise, present it conversationally: collection summaries, active plan progress,
attention items (stalled/overdue), and suggested next actions.
