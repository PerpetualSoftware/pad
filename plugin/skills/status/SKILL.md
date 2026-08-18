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

**Connection header (show this first, above the dashboard).** Also run
`pad session status --format json` and lead with a one-line connection state, so a user
who types `/pad:status` after reading the push docs sees whether this session is wired
to Pad's live stream:
- `announced_armed` true → "Set to accept pushes (armed)." `announced_armed` means
  consent is on, NOT that a monitor is connected — so only add a live claim from the
  server: if `server_reachable` is true, "server sees `accepting_sessions` of
  `connected_sessions` sessions accepting pushes"; if false, "(server not reachable)".
- `local_state` is `disarmed` **and** `auto_arm` is true → "Disconnected this session
  (auto_arm will re-arm the next session)."
- `local_state` is `error` → "Local push state unreadable — failing closed; run
  `/pad:connect` to reset."
- otherwise not accepting → "Not accepting pushes — run `/pad:connect` to receive them
  here."

Keep it to one line; it's a header, not the main event. If `pad session status` fails
(e.g. padd unreachable), skip the header rather than blocking the dashboard.
