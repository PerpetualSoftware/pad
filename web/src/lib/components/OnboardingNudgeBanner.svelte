<script lang="ts">
	import { browser } from '$app/environment';

	interface Props {
		wsSlug: string;
		/**
		 * Click handler that opens the workspace's ConnectWorkspaceModal.
		 * Wired by the parent so this banner stays unaware of how the
		 * modal is mounted (the workspace page already mounts a single
		 * ConnectWorkspaceModal instance — passing a handler avoids
		 * mounting a second one here).
		 */
		onconnect: () => void;
		ondismiss?: () => void;
	}

	let { wsSlug, onconnect, ondismiss }: Props = $props();

	// Dismissal persists in the existing localStorage key shape
	// (`pad-onboarding-dismissed-{wsSlug}`) per IDEA-1516 §3 — users who
	// dismissed the old OnboardingChecklist don't get re-prompted by the
	// new banner. The parent (+page.svelte) owns the canonical dismiss
	// state; this component just signals up via ondismiss.

	function handleDismiss(event: MouseEvent) {
		event.stopPropagation();
		ondismiss?.();
		if (browser) {
			localStorage.setItem(`pad-onboarding-dismissed-${wsSlug}`, 'true');
		}
	}

	function handleConnect() {
		onconnect();
	}

	function handleKeydown(e: KeyboardEvent) {
		// Banner-level Enter/Space activates the Connect CTA — but only
		// when the keydown originates from the banner itself, not from a
		// nested button (the dismiss X). Mirrors ConnectBanner.svelte's
		// guard so dismissing via keyboard doesn't also fire connect.
		if (e.target !== e.currentTarget) return;
		if (e.key === 'Enter' || e.key === ' ') {
			e.preventDefault();
			onconnect();
		}
	}
</script>

<div
	class="nudge-banner"
	role="button"
	tabindex="0"
	onclick={handleConnect}
	onkeydown={handleKeydown}
>
	<span class="nudge-icon" aria-hidden="true">✨</span>
	<div class="nudge-body">
		<span class="nudge-title">Set up your workspace</span>
		<p class="nudge-text">
			Your workspace is ready. Connect your agent, then just say
			<code>set up my workspace</code> &mdash; it'll walk you through setup.
		</p>
	</div>
	<span class="nudge-actions">
		<span class="nudge-cta">Connect agent &rarr;</span>
		<button
			class="dismiss-btn"
			type="button"
			aria-label="Dismiss banner"
			onclick={handleDismiss}
		>
			&#10005;
		</button>
	</span>
</div>

<style>
	/* Mirrors ConnectBanner.svelte's visual language — same border, hover,
	   and focus treatment so the two banners feel like a single system. The
	   nudge-banner is taller because it carries a heading + body, whereas
	   ConnectBanner is a single-line nudge. */
	.nudge-banner {
		display: flex;
		align-items: flex-start;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-3) var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		cursor: pointer;
		transition: border-color 0.15s, background 0.15s;
		box-sizing: border-box;
		text-align: left;
	}
	.nudge-banner:hover {
		border-color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 4%, var(--bg-secondary));
	}
	.nudge-banner:focus-visible {
		outline: 2px solid var(--accent-blue);
		outline-offset: 2px;
	}

	.nudge-icon {
		font-size: 1.2em;
		line-height: 1.3;
		flex-shrink: 0;
		color: var(--accent-blue);
	}

	.nudge-body {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.nudge-title {
		font-size: 0.92em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.nudge-text {
		margin: 0;
		font-size: 0.85em;
		color: var(--text-secondary);
		line-height: 1.45;
	}
	.nudge-text code {
		background: var(--bg-tertiary);
		padding: 1px 5px;
		border-radius: var(--radius-sm);
		font-size: 0.95em;
	}

	.nudge-actions {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-shrink: 0;
		padding-top: 2px;
	}
	.nudge-cta {
		color: var(--accent-blue);
		font-weight: 600;
		font-size: 0.88em;
		white-space: nowrap;
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

	@media (max-width: 480px) {
		.nudge-banner {
			flex-wrap: wrap;
			gap: var(--space-2);
		}
		.nudge-actions {
			width: 100%;
			justify-content: space-between;
		}
	}
</style>
