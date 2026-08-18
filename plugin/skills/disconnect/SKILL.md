---
description: Disconnect this session from Pad's push stream — stop receiving pushes for the rest of this session
allowed-tools: ["Bash"]
---

`/pad:disconnect` withdraws THIS session's consent to receive `pad push` notifications.
It holds for the rest of the session even if the repo opted in via
`.pad.toml push.auto_arm` — a disconnect must not be a lie.

If the `pad` CLI is missing (command not found), say so and point at
https://getpad.dev/install. Otherwise ALWAYS run the disarm — do NOT gate it on finding
a `.pad.toml`. Consent is scoped to this SESSION (keyed by the session, not the
workspace), so a session that connected in one repo must be able to disconnect from
anywhere, including a directory with no linked workspace; refusing to disarm there would
leave it receiving pushes.

Steps:

1. Run `pad session disarm`. This writes a session-scoped explicit-off marker (it does
   not delete config), so the session stops accepting pushes now and stays that way for
   its remaining life. It works regardless of the current directory's workspace.

2. Tell the user they're disconnected. If the command reported that the repo has
   `auto_arm` on, add the honest caveat: this session is disconnected, but a NEW session
   in this repo will connect again — to turn that off permanently, remove
   `push.auto_arm` from `.pad.toml` (the same deliberate edit that turned it on).

Note: the monitor re-checks consent every couple of seconds and drops the open stream
when it sees the disarm, so delivery stops within a brief window rather than lingering.
Run `/pad:status` to confirm the state.
