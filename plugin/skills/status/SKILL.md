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
- `announced_armed` true → "Connected — this session accepts pushes." If the server was
  reachable, add the counts (`connected_sessions` connected, `accepting_sessions`
  accepting pushes).
- `local_state` is `disarmed` **and** `auto_arm` is true → "Disconnected this session
  (auto_arm will re-arm the next session)."
- otherwise not accepting → "Not connected — run `/pad:connect` to receive pushes here."

Keep it to one line; it's a header, not the main event. If `pad session status` fails
(e.g. padd unreachable), skip the header rather than blocking the dashboard.
