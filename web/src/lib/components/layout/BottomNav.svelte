<!--
	BottomNav — mobile-only persistent bottom navigation (PLAN-1694).

	Five slots: Dashboard · Search · Quick-capture (center ＋) · Activity · You.
	The "You" slot opens YouSheet (workspace switch + nav overflow + account) —
	together with this bar it replaces the retired mobile <TopBar /> inside a
	workspace (PLAN-1694 Phase 2-3).

	Rendered in the workspace layout ([username]/[workspace]/+layout.svelte) so
	it only appears inside a workspace. Shows only on mobile (uiStore.isMobile,
	≤768px). While mounted on mobile it toggles `body.has-bottom-nav`, which
	app.css uses to pad .main-content so content can't hide under the fixed bar.

	Active-state comes from the shared nav source ($lib/nav/destinations) — the
	same module the Sidebar uses — so the surfaces can't drift (DR-1).
-->
<script lang="ts">
	import { onDestroy } from 'svelte';
	import { page } from '$app/state';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import QuickCaptureSheet from '$lib/components/layout/QuickCaptureSheet.svelte';
	import YouSheet from '$lib/components/layout/YouSheet.svelte';
	import { getActiveKey } from '$lib/nav/destinations';

	let wsSlug = $derived(workspaceStore.current?.slug);
	let wsUsername = $derived(workspaceStore.current?.owner_username ?? '');
	let wsPrefix = $derived(wsUsername && wsSlug ? `/${wsUsername}/${wsSlug}` : '');
	let activeKey = $derived(getActiveKey(page.url.pathname, wsPrefix));

	let youOpen = $state(false);
	let captureOpen = $state(false);

	// Keys with their own primary slot. "You" holds everything else (the nav
	// overflow), so highlight it whenever the active page lives behind it.
	const PRIMARY_SLOT_KEYS = ['dashboard', 'activity'];
	let youActive = $derived(!!activeKey && !PRIMARY_SLOT_KEYS.includes(activeKey));

	// Drive the .main-content reflow only while the bar is actually shown
	// (mobile). app.css owns the media-gated padding rule.
	$effect(() => {
		if (typeof document === 'undefined') return;
		document.body.classList.toggle('has-bottom-nav', uiStore.isMobile);
	});
	onDestroy(() => {
		if (typeof document !== 'undefined') document.body.classList.remove('has-bottom-nav');
	});
</script>

{#if uiStore.isMobile && wsPrefix}
	<nav class="bottom-nav" aria-label="Primary">
		<a
			class="bn-item"
			class:active={activeKey === 'dashboard'}
			href={wsPrefix}
			onclick={() => uiStore.onNavigate()}
		>
			<span class="bn-icon" aria-hidden="true">📊</span>
			<span class="bn-label">Dashboard</span>
		</a>

		<button
			class="bn-item"
			type="button"
			onclick={() => {
				uiStore.openSearch();
				uiStore.onNavigate();
			}}
		>
			<span class="bn-icon" aria-hidden="true">🔍</span>
			<span class="bn-label">Search</span>
		</button>

		<button
			class="bn-item bn-capture"
			type="button"
			onclick={() => (captureOpen = true)}
			aria-label="Quick capture"
		>
			<span class="bn-capture-plus" aria-hidden="true">＋</span>
		</button>

		<a
			class="bn-item"
			class:active={activeKey === 'activity'}
			href={`${wsPrefix}/activity`}
			onclick={() => uiStore.onNavigate()}
		>
			<span class="bn-icon" aria-hidden="true">📋</span>
			<span class="bn-label">Activity</span>
		</a>

		<button
			class="bn-item"
			class:active={youActive || youOpen}
			type="button"
			onclick={() => (youOpen = true)}
			aria-haspopup="dialog"
			aria-expanded={youOpen}
		>
			<span class="bn-icon" aria-hidden="true">👤</span>
			<span class="bn-label">You</span>
		</button>
	</nav>

	<YouSheet open={youOpen} onclose={() => (youOpen = false)} />

	<QuickCaptureSheet
		open={captureOpen}
		onclose={() => (captureOpen = false)}
		{wsSlug}
		{wsPrefix}
	/>
{/if}

<style>
	.bottom-nav {
		position: fixed;
		bottom: 0;
		left: 0;
		right: 0;
		z-index: 40; /* above TopBar(35)/sidebar(30)/backdrop(25), below sheets(50) */
		display: flex;
		align-items: stretch;
		height: calc(var(--bottom-nav-height) + env(safe-area-inset-bottom, 0px));
		padding-bottom: env(safe-area-inset-bottom, 0px);
		background: var(--bg-secondary);
		border-top: 1px solid var(--border);
	}
	.bn-item {
		flex: 1;
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: 2px;
		background: none;
		border: none;
		cursor: pointer;
		padding: 0;
		color: var(--text-muted);
		text-decoration: none;
		font-family: var(--font-ui);
		min-width: 0;
	}
	.bn-item.active {
		color: var(--text-primary);
	}
	.bn-icon {
		font-size: 1.2em;
		line-height: 1;
	}
	.bn-label {
		font-size: 0.68em;
		font-weight: 500;
		max-width: 100%;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	/* Center capture action — a raised pill so it reads as the primary action. */
	.bn-capture {
		flex: 0 0 auto;
		width: 56px;
	}
	.bn-capture-plus {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 44px;
		height: 44px;
		border-radius: 50%;
		background: var(--accent-blue);
		color: #fff;
		font-size: 1.4em;
		line-height: 1;
		box-shadow: 0 2px 8px rgba(0, 0, 0, 0.35);
	}
</style>
