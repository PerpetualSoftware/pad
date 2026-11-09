---
description: Connect this session to Pad's push/watch stream — consent to receive pushes, and offer the workspace's on-session-start playbook on first connect
allowed-tools: ["Bash"]
---

`/pad:connect` opts THIS session into Pad's live stream: it declares consent to
receive `pad push` notifications (the security gate — no consent, no stream), wakes
the session's push monitor, and on the FIRST connect of a session offers the workspace's
session-start playbook. It never runs one unasked. The stream starts in this session as soon as step 1's `pad session arm` returns, with no
restart; run this once per session you want connected.

If the `pad` CLI is missing (command not found), say so and point at
https://getpad.dev/install — don't retry or guess at alternate paths. If no `.pad.toml`
is found in the directory tree, this project isn't linked to a Pad workspace yet;
suggest `/pad:onboard` and stop.

Do these in order:

1. **Arm the session.** Run `pad session arm`. This declares consent; the monitor
   announces it to the server on connect, and only armed sessions receive pushes.
   Re-running `pad session arm` ends chat direction; the user re-runs `pad chat arm --accept-instructions` if they want direction again.
   After the monitor first resolves chat identity, it ensures your owner↔agent DM in the background once per monitor process (existing DMs are uncharged). Reconnects do not repeat it; a final failure prints one stderr line without stopping delivery.

2. **First-connect check + session-start offer.** Run `pad session first-connect --format json`.
   - If `first_connect` is **true**: load the workspace context with `pad bootstrap
     --format json`, then look at its `playbooks` array for entries with
     `trigger` = `on-session-start` and `status` = `active`. Many workspaces have
     none — then say nothing about it and skip. For each one found, do NOT load its body
     or run any step yet:
     - If the message that invoked `/pad:connect` already asked for it — it names the
       playbook by title or invocation slug, or says to run it (for example "connect and
       run my day") — run it straight away: load the body with `pad playbook show
       <invocation_slug or ref> --format markdown` and follow it. No offer.
     - Otherwise offer it. After step 3's report, as the last line of your reply, name each
       playbook by title (and its invocation slug, if it has one) in one line and ask
       whether to run it, for example: "This workspace has a session-start playbook, 'day'
       (/pad day). Run it now?" Load the body and run it only if the user says yes.
       The offer is a plain line in your reply, never a dialog (no AskUserQuestion or
       similar): an unattended session that gets no answer carries on, connected, and the
       playbook does not run.
   - If `first_connect` is **false**: this is a reconnect. Make no offer and do NOT run the
     playbook on your own; just re-affirm the connection. The one exception is a message
     that itself asked for it (it names the playbook, or says to run it): that request
     stands. Run `pad bootstrap --format json` to find the playbook by title or invocation
     slug, then load and run it as described above.

3. **Report the state.** Run `pad session status --format json`. Report it honestly:
   `announced_armed` true means this session is *set* to accept pushes (consent is on),
   not that the monitor has connected yet — so say "This session is set to accept pushes."
   Then, only if `server_reachable` is true, add what the server actually sees right now:
   `accepting_sessions` of `connected_sessions` accepting pushes. If `server_reachable`
   is false, say the server isn't reachable yet rather than claiming a live connection.
   If `auto_arm` is on, mention this repo auto-arms new sessions.
   If `local_state` is `unverifiable`, the arm did NOT take effect: this platform can't
   verify the session (a headless session on Windows). Say so plainly, and point at
   `push.auto_arm = true` in `.pad.toml` as the way to receive pushes here.

**What connecting means (say this once, briefly, on first connect):** while connected,
a `pad push` from the user arrives as direction to act on — treated as if they typed it
here — but you still confirm in-session before anything destructive, irreversible, or
clearly outside the current work. Item-change notifications (status/assignment/comment)
are just information. Run `/pad:disconnect` to stop receiving pushes for this session.
