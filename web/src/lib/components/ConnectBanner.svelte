<script lang="ts">
	import { browser } from '$app/environment';
	import { api } from '$lib/api/client';
	import ConnectWorkspaceModal from './ConnectWorkspaceModal.svelte';

	interface Props {
		wsSlug: string;
		serverUrl: string;
		workspaceName?: string;
	}

	let { wsSlug, serverUrl, workspaceName = '' }: Props = $props();

	// `null` = unknown (still loading, banner stays hidden to avoid a flash);
	// `true` = workspace already has at least one CLI-sourced item, auto-hide;
	// `false` = no CLI source detected, show the banner.
	let hasCliSource = $state<boolean | null>(null);
	let dismissed = $state(false);
	let connectOpen = $state(false);

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
			dismissed =
				localStorage.getItem(`pad-cli-banner-dismissed-${wsSlug}`) === 'true';
		}
	});

	// Effect B — fetch `has_cli_source` from the dashboard whenever the
	// workspace slug changes. Reset to `null` first so we don't briefly
	// render the banner for the new workspace based on the previous
	// workspace's signal. Failures fall back to `false` (fail-open: better
	// to show the banner than to hide it on a transient error).
	$effect(() => {
		if (!wsSlug) return;
		hasCliSource = null;
		api.dashboard
			.get(wsSlug)
			.then((d) => {
				hasCliSource = d.has_cli_source;
			})
			.catch(() => {
				hasCliSource = false;
			});
	});

	let visible = $derived(
		!!wsSlug && !dismissed && hasCliSource === false && !connectOpen
	);

	function dismiss(event: MouseEvent) {
		event.stopPropagation();
		dismissed = true;
		if (browser) {
			localStorage.setItem(`pad-cli-banner-dismissed-${wsSlug}`, 'true');
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
			if (e.key === 'Enter' || e.key === ' ') {
				e.preventDefault();
				openModal();
			}
		}}
	>
		<span class="banner-icon" aria-hidden="true">
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
		</span>
		<span class="banner-text">
			Manage this workspace from your terminal &mdash; get the CLI &rarr;
		</span>
		<span class="banner-actions">
			<span class="banner-cta">Get the CLI</span>
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
