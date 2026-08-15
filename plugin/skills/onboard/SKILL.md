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
  or `pad workspace init` yourself — `pad workspace init` drops into a
  browser-based auth flow and polls for a human to approve it, blocking your
  tool call for minutes with no non-interactive fallback (the server expires
  that session after ~5 minutes, ~20 for first-admin setup, so it does end —
  but the poll has no limit of its own and only stops when the server says so).
  `pad init` is no safer as a probe — verified live, it drops into the same
  browser login and blocks identically. Tell the user to run `pad init`
  themselves in an interactive terminal — in Claude Code,
  suggest they type `! pad init` to run it directly in their own terminal.
  Stop here; there's nothing more this skill can do until that completes.

Once linked, run `pad bootstrap --format json`:
- If `needs_onboarding` is true, offer the workspace onboarding flow — scan
  the codebase (README, build config, CI), suggest starter conventions with
  the project's real commands, and propose an initial plan. Create items only
  after the user confirms the shape.
- If `needs_onboarding` is false, this workspace is already set up — say so
  and briefly summarize what exists (e.g. how many collections and active
  conventions, from the bootstrap payload's `collections` and
  `convention_index` arrays), then offer the extend/audit flow instead of
  re-running first-time setup: add a new section (collection, convention,
  role, or playbook), review existing conventions against the current
  codebase, or do nothing if everything already looks right. Same
  confirm-before-creating rule as everywhere else — never assume the
  extend/audit answer.
