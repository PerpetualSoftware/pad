---
description: Connect this session to Pad's push/watch stream — consent to receive pushes, and run the workspace's on-session-start ritual on first connect
allowed-tools: ["Bash"]
---

`/pad:connect` opts THIS session into Pad's live stream: it declares consent to
receive `pad push` notifications (the security gate — no consent, no stream), wakes
the session's push monitor, and on the FIRST connect of a session runs the workspace's
boot ritual. Invoking this skill is also what starts the gated monitor process, so run
it once per session you want connected.

If the `pad` CLI is missing (command not found), say so and point at
https://getpad.dev/install — don't retry or guess at alternate paths. If no `.pad.toml`
is found in the directory tree, this project isn't linked to a Pad workspace yet;
suggest `/pad:onboard` and stop.

Do these in order:

1. **Arm the session.** Run `pad session arm`. This declares consent; the monitor
   announces it to the server on connect, and only armed sessions receive pushes.

2. **First-connect check + boot ritual.** Run `pad session first-connect --format json`.
   - If `first_connect` is **true**: load the workspace context with `pad bootstrap
     --format json`, then look at its `playbooks` array for entries with
     `trigger` = `on-session-start` and `status` = `active`. For each, load the body
     with `pad playbook show <invocation_slug> --format markdown` and follow it. This is
     the workspace's own operating ritual firing for whoever connects. (Many workspaces
     have none — that's fine; just skip.)
   - If `first_connect` is **false**: this is a reconnect. Do NOT re-run the boot ritual;
     just re-affirm the connection.

3. **Report the state.** Run `pad session status --format json`. Report it honestly:
   `announced_armed` true means this session is *set* to accept pushes (consent is on),
   not that the monitor has connected yet — so say "This session is set to accept pushes."
   Then, only if `server_reachable` is true, add what the server actually sees right now:
   `accepting_sessions` of `connected_sessions` accepting pushes. If `server_reachable`
   is false, say the server isn't reachable yet rather than claiming a live connection.
   If `auto_arm` is on, mention this repo auto-arms new sessions.

**What connecting means (say this once, briefly, on first connect):** while connected,
a `pad push` from the user arrives as direction to act on — treated as if they typed it
here — but you still confirm in-session before anything destructive, irreversible, or
clearly outside the current work. Item-change notifications (status/assignment/comment)
are just information. Run `/pad:disconnect` to stop receiving pushes for this session.
