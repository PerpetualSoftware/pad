# WebMCP browser surface

Browser-side WebMCP layer (PLAN-1888) that registers Pad's catalog tools with
`document.modelContext.registerTool()` so browser-native AI agents can call them
against the logged-in web session. Gated behind the opt-in `webmcp_enabled`
platform setting (default off); native-API-only with feature detection.

Module pieces:

- `descriptors.ts` — pure tool-surface JSON → `ModelContextTool` builder.
- `dispatch.ts` — `(tool, action, args) → client.ts`, forcing the route `wsSlug`.
- `register.ts` — lifecycle: fetch surface, build, register, unregister.
- `types.d.ts` — `document.modelContext` type stub.

## Tool annotations (DR-2)

Two browser-only annotations are derived in `descriptors.ts`:

- **`readOnlyHint`** — set only when *every* action a tool exposes is a read
  (`isAllReadOnly`). Mixed tools (e.g. `pad_item`) omit it, so the host prompts
  for per-invocation consent on every call.
- **`untrustedContentHint`** — set for every tool whose output can echo
  user-authored workspace content (`surfacesUntrustedContent`): a tool surfaces
  content unless **every** action it exposes is content-free server
  introspection (`server-info` / `version` / `tool-surface`). Tells the agent to
  treat the result as unverified input — prompt-injection hardening against a
  malicious item body smuggling instructions back to the agent. **Note:**
  `pad_meta` DOES carry the hint — despite its name it also exposes `bootstrap`,
  which returns the workspace bootstrap blob (user + workspace content). All 9
  catalog tools therefore carry it today; the exemption only applies to a
  hypothetical pure version/meta surface.

## Consent manual-verification checklist

The Chrome WebMCP origin-trial consent behavior can't be exercised in CI (no
headless origin-trial harness), so verify it by hand after touching the
descriptor/annotation layer. Run in **Chrome 149+** with the WebMCP origin
trial enabled and the platform setting `webmcp_enabled` turned on, signed in to
a workspace.

1. **Read tools run quietly.** Have the browser agent call an **all-read** tool
   — one whose every action is a read (e.g. `pad_search action=query`,
   `pad_project action=dashboard`). It should execute **without** a consent
   prompt. These carry `readOnlyHint: true`. NB: `readOnlyHint` is per-*tool*,
   so a read action on a *mixed* tool (e.g. `pad_item action=list`) still
   prompts — `pad_item` carries no `readOnlyHint` because it also exposes
   writes. Verify that too: `pad_item action=list` should prompt.
2. **Mutating tools prompt for consent.** Have the agent call a write action on
   a mixed tool — `pad_item action=create` and `pad_item action=delete`. Each
   call must trigger a **per-invocation consent prompt** before it runs.
   Declining must abort the call (no item created/deleted). Mixed tools carry
   **no** `readOnlyHint`, which is what makes the host prompt.
3. **No `workspace` arg is offered (DR-4).** Inspect the tools the agent sees
   (its tool list / schema). No tool's `inputSchema` should expose a
   `workspace` property — it's stripped in `buildDescriptor`. Confirm the agent
   cannot target a workspace other than the current route's; every dispatch is
   forced to the route `wsSlug`.
4. **Untrusted-content honesty.** Confirm content-surfacing tool results are
   flagged with `untrustedContentHint: true` (e.g. via the agent host's tool
   inspector). Every catalog tool — including `pad_meta`, whose `bootstrap`
   action returns workspace content — carries the hint today.

Record the result (pass/fail per step + Chrome version) in the PR or task when
re-verifying.
