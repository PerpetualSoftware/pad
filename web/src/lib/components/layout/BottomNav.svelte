<!--
	BottomNav — mobile-only persistent bottom navigation (PLAN-1694).

	Five slots: Dashboard · Search · Quick-capture (center ＋) · Activity · More.
	Coexists with the existing mobile <TopBar /> this session; retiring the
	header + consolidating workspace/account into a "You" sheet is a deferred
	design checkpoint (PLAN-1694, DR-3).

	Rendered in the workspace layout ([username]/[workspace]/+layout.svelte) so
	it only appears inside a workspace, never on login/console pages. Shows
	itself only on mobile (uiStore.isMobile, ≤768px). While mounted on mobile it
	toggles `body.has-bottom-nav`, which app.css uses to pad .main-content so
	content can't hide under the fixed bar.

	Nav destinations + active-state come from the shared source
	($lib/nav/destinations) — the same module the Sidebar uses — so the two nav
	surfaces can't drift (DR-1).
-->
<script lang="ts">
	import { onDestroy } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import QuickCaptureSheet from '$lib/components/layout/QuickCaptureSheet.svelte';
	import { getPrimaryDestinations, getActiveKey } from '$lib/nav/destinations';

	let wsSlug = $derived(workspaceStore.current?.slug);
	let wsUsername = $derived(workspaceStore.current?.owner_username ?? '');
	let wsPrefix = $derived(wsUsername && wsSlug ? `/${wsUsername}/${wsSlug}` : '');
	let isGuest = $derived(workspaceStore.current?.is_guest ?? false);
	let activeKey = $derived(getActiveKey(page.url.pathname, wsPrefix));

	let moreOpen = $state(false);
	let captureOpen = $state(false);

	// Keys promoted to their own primary slots — excluded from the More sheet.
	const PRIMARY_SLOT_KEYS = ['dashboard', 'activity'];

	// Overflow destinations for the More sheet: the static destinations not
	// promoted to a slot, with guest-hidden ones (Settings) dropped for guests.
	let overflowDestinations = $derived(
		getPrimaryDestinations(wsPrefix)
			.filter((d) => !PRIMARY_SLOT_KEYS.includes(d.key))
			.filter((d) => !(d.guestHidden && isGuest))
	);

	// Collections, split like the Sidebar (regular vs agent/system).
	const agentSlugs = ['conventions', 'playbooks'];
	let regularCollections = $derived(
		collectionStore.collections.filter((c) => !agentSlugs.includes(c.slug))
	);
	let agentCollections = $derived(
		collectionStore.collections.filter((c) => agentSlugs.includes(c.slug))
	);

	// Highlight "More" whenever the active page lives behind it (i.e. not a
	// primary slot and not nothing) so the bar reflects where you are.
	let moreActive = $derived(
		!!activeKey && !PRIMARY_SLOT_KEYS.includes(activeKey)
	);

	// Drive the .main-content reflow only while the bar is actually shown
	// (mobile). app.css owns the media-gated padding rule.
	$effect(() => {
		if (typeof document === 'undefined') return;
		document.body.classList.toggle('has-bottom-nav', uiStore.isMobile);
	});
	onDestroy(() => {
		if (typeof document !== 'undefined') document.body.classList.remove('has-bottom-nav');
	});

	function navigate(href: string) {
		moreOpen = false;
		uiStore.onNavigate();
		goto(href);
	}
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
			class:active={moreActive || moreOpen}
			type="button"
			onclick={() => (moreOpen = true)}
			aria-haspopup="dialog"
			aria-expanded={moreOpen}
		>
			<span class="bn-icon" aria-hidden="true">☰</span>
			<span class="bn-label">More</span>
		</button>
	</nav>

	<BottomSheet open={moreOpen} onclose={() => (moreOpen = false)} title="More">
		<div class="more-sheet">
			{#each overflowDestinations as dest (dest.key)}
				<button
					class="more-item"
					class:active={activeKey === dest.key}
					type="button"
					onclick={() => navigate(dest.href)}
				>
					<span class="more-icon" aria-hidden="true">{dest.icon}</span>
					<span>{dest.label}</span>
				</button>
			{/each}

			{#if regularCollections.length}
				<div class="more-section">Collections</div>
				{#each regularCollections as coll (coll.id)}
					<button
						class="more-item"
						class:active={activeKey === `collection:${coll.slug}`}
						type="button"
						onclick={() => navigate(`${wsPrefix}/${coll.slug}`)}
					>
						<span class="more-icon" aria-hidden="true">{coll.icon}</span>
						<span>{coll.name}</span>
					</button>
				{/each}
			{/if}

			{#if agentCollections.length}
				<div class="more-section">Agent</div>
				{#each agentCollections as coll (coll.id)}
					<button
						class="more-item"
						class:active={activeKey === `collection:${coll.slug}`}
						type="button"
						onclick={() => navigate(`${wsPrefix}/${coll.slug}`)}
					>
						<span class="more-icon" aria-hidden="true">{coll.icon}</span>
						<span>{coll.name}</span>
					</button>
				{/each}
			{/if}
		</div>
	</BottomSheet>

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

	.more-sheet {
		display: flex;
		flex-direction: column;
		padding: 0 var(--space-3) var(--space-2);
	}
	.more-item {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		background: none;
		border: none;
		cursor: pointer;
		padding: var(--space-3) var(--space-3);
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		font-size: 0.98em;
		font-family: var(--font-ui);
		text-align: left;
	}
	.more-item:hover {
		background: var(--bg-hover);
	}
	.more-item.active {
		color: var(--text-primary);
		background: var(--bg-hover);
		font-weight: 600;
	}
	.more-icon {
		font-size: 1.1em;
		width: 1.4em;
		text-align: center;
		flex-shrink: 0;
	}
	.more-section {
		padding: var(--space-3) var(--space-3) var(--space-1);
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}
</style>
