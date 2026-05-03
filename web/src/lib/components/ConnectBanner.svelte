<script lang="ts">
	import { browser } from '$app/environment';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import ConnectMCPModal from './ConnectMCPModal.svelte';
	import ConnectWorkspaceModal from './ConnectWorkspaceModal.svelte';

	interface Props {
		wsSlug: string;
		serverUrl: string;
		workspaceName?: string;
	}

	let { wsSlug, serverUrl, workspaceName = '' }: Props = $props();

	// `null` = unknown (still loading, banner stays hidden to avoid a flash);
	// `true` = workspace already has at least one agent-sourced item (CLI
	//          or Remote MCP), auto-hide;
	// `false` = no agent activity detected, show the banner.
	let hasAgentActivity = $state<boolean | null>(null);
	let dismissed = $state(false);
	// Two independent modal states because the user can flip between them
	// via "Prefer the CLI? →" in the MCP modal — both can be closed, only
	// one open at a time. The visibility predicate ORs both so the banner
	// hides while either is mounted.
	let mcpOpen = $state(false);
	let cliOpen = $state(false);
	let connectOpen = $derived(mcpOpen || cliOpen);

	// Banner mode is gated on the server-exposed MCP public URL rather than
	// `cloudMode` directly: it's true on Pad Cloud AND on any self-host that
	// opted into Remote MCP via PAD_MCP_PUBLIC_URL. When the server has no
	// public MCP URL configured, fall back to the CLI install flow that
	// has historically been the only way to wire up an agent.
	let mode = $derived<'mcp' | 'cli'>(authStore.mcpPublicUrl !== '' ? 'mcp' : 'cli');

	// localStorage dismiss key migration (TASK-1114). Existing users have
	// `pad-cli-banner-dismissed-{ws}` set; we now write to the broader
	// `pad-connect-banner-dismissed-{ws}` so a future banner-mode rename
	// doesn't force users to re-dismiss. For one release, read both keys
	// so existing dismissals carry over without re-pestering. After ~1
	// release we can drop the legacy-key read.
	const NEW_DISMISS_KEY = (slug: string) => `pad-connect-banner-dismissed-${slug}`;
	const LEGACY_DISMISS_KEY = (slug: string) => `pad-cli-banner-dismissed-${slug}`;

	// Effect A — sync dismissed state from localStorage when workspace
	// changes. Mirrors the pattern in `[username]/[workspace]/+page.svelte`
	// for the onboarding-dismissed flag. Per CONVE-606, kept separate from
	// the data fetch below so a route change doesn't entangle two unrelated
	// reactive lifecycles.
	//
	// TODO: persistence is per-browser today. If we need cross-device dismiss
	// state we can back this with a `workspace_user_state` row.
	$effect(() => {
		if (browser && wsSlug) {
			const isDismissed =
				localStorage.getItem(NEW_DISMISS_KEY(wsSlug)) === 'true' ||
				localStorage.getItem(LEGACY_DISMISS_KEY(wsSlug)) === 'true';
			dismissed = isDismissed;
		}
	});

	// Fetch `has_agent_activity` for a specific slug. Two safeguards
	// against stale responses overwriting fresher ones:
	//
	// 1. A monotonic sequence id captured at call time — only the LATEST
	//    request's result is applied. This handles same-workspace races,
	//    e.g. an in-flight workspace-change fetch that resolves AFTER the
	//    modal-close refetch (which would otherwise stomp the newer
	//    `true` with the older `false`).
	// 2. A captured slug recheck — if the user has already navigated to a
	//    different workspace by the time the response lands, drop it.
	//
	// Failures fall back to `false` (fail-open: better to show the banner
	// than to hide it on a transient error).
	let fetchSeq = 0;
	function refreshHasAgentActivity(slug: string) {
		if (!slug) return;
		const mySeq = ++fetchSeq;
		hasAgentActivity = null;
		api.dashboard
			.get(slug)
			.then((d) => {
				if (mySeq !== fetchSeq) return; // a newer request superseded us
				if (slug !== wsSlug) return; // workspace changed under us
				hasAgentActivity = d.has_agent_activity;
			})
			.catch(() => {
				if (mySeq !== fetchSeq) return;
				if (slug !== wsSlug) return;
				hasAgentActivity = false;
			});
	}

	// Effect B — refetch on workspace change. Per CONVE-606, kept separate
	// from the localStorage sync above.
	$effect(() => {
		refreshHasAgentActivity(wsSlug);
	});

	// Effect C — refetch when the CLI modal CLOSES (regardless of banner
	// mode). The user's typical CLI flow is: open modal → copy command →
	// run it in their terminal → close the modal. By that point an
	// agent-sourced item has almost certainly landed; the fresh fetch
	// makes the banner auto-hide in-session instead of waiting for a
	// route change.
	//
	// We gate on the CLI modal specifically (not `connectOpen`) because:
	//
	// - MCP modal close: the user has gone off to a separate agent client
	//   (Claude Desktop / Cursor / Windsurf) to paste the URL — refetching
	//   right now doesn't help; their first MCP-sourced item lands minutes
	//   later via SSE / Effect B on the next visit.
	// - CLI modal close in MCP mode: reachable via "Prefer the CLI? →" in
	//   the MCP modal. If the user switched to CLI and ran the install,
	//   the same refetch logic applies — refetching helps.
	let prevCliOpen = $state(false);
	$effect.pre(() => {
		if (prevCliOpen && !cliOpen) {
			refreshHasAgentActivity(wsSlug);
		}
		prevCliOpen = cliOpen;
	});

	let visible = $derived(
		!!wsSlug && !dismissed && hasAgentActivity === false && !connectOpen
	);

	function dismiss(event: MouseEvent) {
		event.stopPropagation();
		dismissed = true;
		if (browser) {
			// Write the new key only. The legacy key, if set, stays in
			// localStorage as harmless dead state — readers OR the two
			// keys (Effect A above), so its presence doesn't affect
			// future behavior. Cleaning it up isn't worth the risk of
			// touching localStorage for keys we don't own conceptually.
			localStorage.setItem(NEW_DISMISS_KEY(wsSlug), 'true');
		}
	}

	function openModal() {
		if (mode === 'mcp') {
			mcpOpen = true;
		} else {
			cliOpen = true;
		}
	}

	// Called from ConnectMCPModal when the user clicks "Prefer the CLI? →".
	// The MCP modal closes itself before invoking this; we just open the
	// CLI modal in response.
	function switchMcpToCli() {
		cliOpen = true;
	}
</script>

{#if visible}
	<div
		class="banner"
		role="button"
		tabindex="0"
		onclick={openModal}
		onkeydown={(e) => {
			// Only react when the keydown originated on the banner itself —
			// otherwise keyboard activation of nested focusable elements
			// (e.g. the dismiss button) would also open the modal.
			if (e.target !== e.currentTarget) return;
			if (e.key === 'Enter' || e.key === ' ') {
				e.preventDefault();
				openModal();
			}
		}}
	>
		<span class="banner-icon" aria-hidden="true">
			{#if mode === 'mcp'}
				<!-- Plug icon — universal "connect a service" affordance. -->
				<svg
					width="16"
					height="16"
					viewBox="0 0 24 24"
					fill="none"
					stroke="currentColor"
					stroke-width="2"
					stroke-linecap="round"
					stroke-linejoin="round"
				>
					<path d="M9 2v6" />
					<path d="M15 2v6" />
					<path d="M6 8h12v4a6 6 0 0 1-12 0z" />
					<path d="M12 18v4" />
				</svg>
			{:else}
				<!-- Terminal-arrow icon — historical CLI affordance. -->
				<svg
					width="16"
					height="16"
					viewBox="0 0 24 24"
					fill="none"
					stroke="currentColor"
					stroke-width="2"
					stroke-linecap="round"
					stroke-linejoin="round"
				>
					<polyline points="4 17 10 11 4 5" />
					<line x1="12" y1="19" x2="20" y2="19" />
				</svg>
			{/if}
		</span>
		<span class="banner-text">
			{#if mode === 'mcp'}
				Connect an AI agent to this workspace &mdash; zero install &rarr;
			{:else}
				Manage this workspace from your terminal &mdash; get the CLI &rarr;
			{/if}
		</span>
		<span class="banner-actions">
			<span class="banner-cta">
				{#if mode === 'mcp'}
					Connect
				{:else}
					Get the CLI
				{/if}
			</span>
			<button
				class="dismiss-btn"
				type="button"
				aria-label="Dismiss banner"
				onclick={dismiss}
			>
				&#10005;
			</button>
		</span>
	</div>
{/if}

<!--
	Both modals are always declared so a transient mode flip while the
	user has one open doesn't unmount it mid-interaction. Each tracks an
	independent `open` state; the parent's `connectOpen` derived value is
	true while EITHER is open (so the banner hides during interaction).

	The MCP modal's "Prefer the CLI? →" link calls `onSwitchToCli`, which
	opens the CLI modal — the MCP modal closes itself first via its own
	internal handler so the two never overlap visually.
-->
<ConnectMCPModal
	bind:open={mcpOpen}
	mcpPublicUrl={authStore.mcpPublicUrl}
	{workspaceName}
	onSwitchToCli={switchMcpToCli}
/>

<ConnectWorkspaceModal
	bind:open={cliOpen}
	{serverUrl}
	workspaceSlug={wsSlug}
	{workspaceName}
/>

<style>
	.banner {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-2) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		cursor: pointer;
		font-size: 0.85em;
		transition: border-color 0.15s, background 0.15s;
		box-sizing: border-box;
	}
	.banner:hover {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 4%, var(--bg-secondary));
	}
	.banner:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: 2px;
	}
	.banner-icon {
		display: flex;
		align-items: center;
		justify-content: center;
		color: var(--accent-blue);
		flex-shrink: 0;
	}
	.banner-text {
		flex: 1;
		min-width: 0;
		color: var(--text-secondary);
	}
	.banner-actions {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-shrink: 0;
	}
	.banner-cta {
		color: var(--accent-blue);
		font-weight: 600;
	}
	.dismiss-btn {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 22px;
		height: 22px;
		padding: 0;
		background: transparent;
		border: none;
		border-radius: var(--radius);
		color: var(--text-muted);
		cursor: pointer;
		font-size: 0.85em;
		line-height: 1;
		transition: background 0.15s, color 0.15s;
	}
	.dismiss-btn:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}
</style>
