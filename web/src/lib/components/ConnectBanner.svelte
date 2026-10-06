<script lang="ts">
	import { browser } from '$app/environment';
	import { api } from '$lib/api/client';
	import { authStore } from '$lib/stores/auth.svelte';
	import { sseService } from '$lib/services/sse.svelte';
	import { onDestroy } from 'svelte';
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

	// Single modal state — the unified ConnectWorkspaceModal (PLAN-1519 /
	// TASK-1525) subsumed the previous dual-modal (MCP-vs-CLI) split into
	// a single tabbed surface. The banner just opens it; the modal decides
	// the default tab based on `mcpPublicUrl` availability.
	let connectOpen = $state(false);

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
	// `quiet` (BUG-3447): an event-driven re-check keeps the banner as it is
	// while the request is out, instead of blanking it, and keeps it on a
	// failure; only an answer changes it.
	function refreshHasAgentActivity(slug: string, quiet = false) {
		if (!slug) return;
		const mySeq = ++fetchSeq;
		if (!quiet) hasAgentActivity = null;
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
				// A quiet failure keeps a KNOWN answer; from unknown (it overtook
				// the first check, which its sequence then discarded) it falls
				// back to false like any failure, or the banner hides for good
				// (codex r3).
				if (!quiet || hasAgentActivity === null) hasAgentActivity = false;
			});
	}

	// Effect B — refetch on workspace change. Per CONVE-606, kept separate
	// from the localStorage sync above.
	$effect(() => {
		refreshHasAgentActivity(wsSlug);
	});

	// Effect C — refetch when the modal CLOSES. After the user has gone
	// off to set up their agent (paste a claim code, run `pad init`, or
	// paste the MCP URL into Claude Desktop), an agent-sourced item is
	// likely about to land. Refetching as soon as the user closes the
	// modal makes the banner auto-hide in-session instead of waiting
	// for the next route change.
	//
	// We can't do the refetch from inside the modal because the modal
	// doesn't know which tab actually triggered the agent connection;
	// any close is good-enough signal. Tracking `prevOpen` is the
	// minimum reactive shape to detect the open → closed transition
	// without re-firing on every render. PLAIN variable (not $state): it's
	// only read/written for edge-detection inside the effect below — making
	// it $state makes the effect self-invalidating, which silently wedges
	// the effect scheduler in prod builds (see BUG-1687 / ShareDialog).
	let prevOpen = false;
	$effect.pre(() => {
		if (prevOpen && !connectOpen) {
			refreshHasAgentActivity(wsSlug);
		}
		prevOpen = connectOpen;
	});

	// LIVE (BUG-3447): the banner used to learn of agent activity only on a
	// workspace change or when its own modal closed, so it stayed up while an
	// agent was visibly creating items. While it is showing, an item creation
	// schedules one quiet re-check, coalesced across a burst; once activity is
	// known, events are ignored.
	const AGENT_RECHECK_MS = 400;
	let recheckTimer: ReturnType<typeof setTimeout> | undefined;
	const unsubscribeItems = browser
		? sseService.onItemEvent((event) => {
				// Unknown (the first check still out) counts: its answer may
				// predate the write (codex r2). Only KNOWN activity stops it.
				if (hasAgentActivity === true || dismissed) return;
				if (event.type !== 'item_created') return;
				// Only for the workspace it was armed in: after a switch it would
				// supersede the next workspace's own re-check (codex r1).
				const armedFor = wsSlug;
				clearTimeout(recheckTimer);
				recheckTimer = setTimeout(() => {
					if (wsSlug === armedFor) refreshHasAgentActivity(armedFor, true);
				}, AGENT_RECHECK_MS);
			})
		: () => {};
	onDestroy(() => {
		unsubscribeItems();
		clearTimeout(recheckTimer);
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
		connectOpen = true;
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
		</span>
		<span class="banner-text">
			Connect an AI agent to this workspace &rarr;
		</span>
		<span class="banner-actions">
			<span class="banner-cta">Connect</span>
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

<ConnectWorkspaceModal
	bind:open={connectOpen}
	{serverUrl}
	workspaceSlug={wsSlug}
	{workspaceName}
	mcpPublicUrl={authStore.mcpPublicUrl}
	mcpAuth={authStore.mcpAuth}
/>

<style>
	/* A card BELOW the desktop tab bar, not against it (TASK-3312): flush,
	   its top border doubled the bar's own and drew a line under the active
	   tab, which is meant to flow into the page. Desktop only: phones have no
	   tab strip, and TASK-3312 leaves them unchanged. */
	@media (min-width: 769px) {
		.banner {
			margin-top: var(--space-2);
		}
	}

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
