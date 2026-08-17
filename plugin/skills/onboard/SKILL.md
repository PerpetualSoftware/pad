---
description: Link this project to a Pad workspace and set it up
allowed-tools: ["Bash", "Read"]
---

Set this project up with Pad. If `pad` is not on PATH, give the install one-liner
from https://getpad.dev/install and stop.

If no `.pad.toml` exists, first run `pad auth whoami` (fast and safe: in
non-interactive use — which is where you run — it returns immediately rather
than waiting on input, and it works regardless of workspace-link state) to
check whether this machine is already configured and authenticated:
- If it reports a real user, it's safe to self-heal: run `pad workspace init`
  (a name/`--template` are optional) to link and create the workspace
  non-interactively.
- If it reports "not configured," "session expired," or anything other than a
  real user, this machine hasn't finished setup yet. Do **not** run `pad init`
  or `pad workspace init` yourself — `pad workspace init` now fails fast,
  non-interactively, with an actionable error instead of hanging
  (BUG-2538/BUG-2577): "not been initialized yet" pointing at `pad auth
  setup`, or "not authenticated" pointing at `pad auth login`. That error
  still means only a human at an interactive terminal can finish it, so
  running it yourself just spends a tool call confirming what `pad auth
  whoami` already told you. (With both stdin AND stdout attached to a real
  terminal it instead drops into the browser auth flow, now bounded at 20
  minutes wall-clock — not a state your tool call is ever in.) `pad init` is
  not a safer probe either: verified live, it fails fast when stdin/stdout
  aren't terminals in both states — genuinely unconfigured, and (since
  BUG-2592) the session-expired sub-case of configured-but-unauthenticated —
  with the same real-terminal caveat as above, so probing it tells you
  nothing `pad auth whoami` didn't. Tell the user to run `pad
  init` themselves in an interactive terminal — in Claude Code, suggest they
  type `! pad init` to run it directly in their own terminal.
  Stop here; there's nothing more this skill can do until that completes.

Once linked, run `pad bootstrap --format json`, then **run the onboard
playbook — do not improvise your own setup script.** The playbook is
workspace-owned and user-editable (PLAN-1496's adaptation posture): a team
that has rewritten theirs must get their version from every surface,
including this shortcut. Load the body and follow it — it IS the script:

```bash
pad playbook show onboard --format markdown
```

If the bootstrap's `playbooks` array has no entry with
`invocation_slug=onboard, status=active`, activate it from the library
first — `pad library activate "Onboard a workspace"` (activate takes the
exact library title; `pad library list` shows it), or the Web UI's
Playbooks → Library — then load it.

The body covers all four modes itself — `build` (blank workspace, build
from scratch), `audit` (templated workspace, adapt the seeds), `revisit`
(already onboarded, change something specific), and `defaults` (escape
hatch) — so route by the bootstrap rather than re-scripting them here:
`needs_onboarding: true` points at build/audit; `false` means the workspace
is already set up, so say so, summarize briefly what exists (collections,
active conventions from `convention_index`), and offer the playbook's
revisit mode rather than re-running first-time setup. Create items only
after the user confirms the shape — the same confirm-before-creating rule
as everywhere else.
