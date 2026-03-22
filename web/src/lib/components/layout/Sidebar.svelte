<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { dndzone } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { api } from '$lib/api/client';
	import type { Collection } from '$lib/types';
	import WorkspaceSwitcher from './WorkspaceSwitcher.svelte';

	let wsSlug = $derived(workspaceStore.current?.slug);
	let isDashboardPage = $derived(wsSlug ? page.url.pathname === `/${wsSlug}` : false);

	let activeCollectionSlug = $derived.by(() => {
		if (!wsSlug) return null;
		const prefix = `/${wsSlug}/`;
		const path = page.url.pathname;
		if (!path.startsWith(prefix)) return null;
		const rest = path.slice(prefix.length);
		const slug = rest.split('/')[0];
		if (slug === 'settings' || slug === 'new' || slug === 'library' || slug === '') return null;
		return slug;
	});

	let activeColl = $derived(
		activeCollectionSlug
			? collectionStore.collections.find(c => c.slug === activeCollectionSlug)
			: null
	);

	let currentTheme = $state<'dark' | 'light'>('dark');

	// Swipe tracking for mobile
	let touchStartX = $state(0);
	let touchCurrentX = $state(0);
	let isSwiping = $state(false);
	let sidebarEl = $state<HTMLElement>();

	const swipeThreshold = 80;

	// Swipe-to-open tracking (separate from swipe-to-close)
	let openSwipeStartX = $state(0);
	let openSwipeStartY = $state(0);
	let openSwipeTracking = $state(false);
	let openSwipeLocked = $state(false); // true once we confirm it's a horizontal swipe
	const openSwipeMinDistance = 50; // minimum rightward distance to trigger open

	// Drag-and-drop reordering state
	let sidebarCollections: Collection[] = $state([]);
	let isDraggingSidebar = $state(false);
	const flipDurationMs = 150;

	const agentSlugs = ['conventions', 'playbooks'];

	let regularCollections = $derived(
		collectionStore.collections.filter(c => !agentSlugs.includes(c.slug))
	);
	let agentCollections = $derived(
		collectionStore.collections.filter(c => agentSlugs.includes(c.slug))
	);

	$effect(() => {
		if (!isDraggingSidebar) {
			sidebarCollections = [...regularCollections];
		}
	});

	function handleCollectionConsider(e: CustomEvent<DndEvent<Collection>>) {
		sidebarCollections = e.detail.items;
		isDraggingSidebar = true;
	}

	async function handleCollectionFinalize(e: CustomEvent<DndEvent<Collection>>) {
		sidebarCollections = e.detail.items;
		isDraggingSidebar = false;

		if (!wsSlug) return;
		for (let i = 0; i < sidebarCollections.length; i++) {
			const coll = sidebarCollections[i];
			if (coll.sort_order !== i) {
				await api.collections.update(wsSlug, coll.slug, { sort_order: i });
			}
		}
		collectionStore.loadCollections(wsSlug);
	}

	onMount(() => {
		const saved = localStorage.getItem('pad-theme');
		if (saved === 'light' || saved === 'dark') {
			currentTheme = saved;
		} else if (window.matchMedia('(prefers-color-scheme: light)').matches) {
			currentTheme = 'light';
		}
		document.documentElement.setAttribute('data-theme', currentTheme);
	});

	function toggleTheme() {
		currentTheme = currentTheme === 'dark' ? 'light' : 'dark';
		document.documentElement.setAttribute('data-theme', currentTheme);
		localStorage.setItem('pad-theme', currentTheme);
	}

	function handleTouchStart(e: TouchEvent) {
		touchStartX = e.touches[0].clientX;
		touchCurrentX = touchStartX;
		isSwiping = true;
	}

	function handleTouchMove(e: TouchEvent) {
		if (!isSwiping) return;
		touchCurrentX = e.touches[0].clientX;

		const delta = touchCurrentX - touchStartX;
		if (delta < 0 && sidebarEl) {
			const translate = Math.max(delta, -300);
			sidebarEl.style.transform = `translateX(${translate}px)`;
			sidebarEl.style.transition = 'none';
		}
	}

	function handleTouchEnd() {
		if (!isSwiping || !sidebarEl) return;
		isSwiping = false;

		const delta = touchCurrentX - touchStartX;
		sidebarEl.style.transform = '';
		sidebarEl.style.transition = '';

		if (delta < -swipeThreshold) {
			uiStore.closeSidebar();
		}
	}
</script>

<!-- Swipe from left side to open (when sidebar is closed on mobile) -->
<!-- Touch zone: 30px from left to 50% viewport width, requires intentional rightward swipe -->
<svelte:window
	ontouchstart={(e) => {
		if (!uiStore.isMobile || uiStore.sidebarOpen) return;
		const x = e.touches[0].clientX;
		const halfVw = window.innerWidth / 2;
		if (x > 20 && x < halfVw) {
			openSwipeStartX = x;
			openSwipeStartY = e.touches[0].clientY;
			openSwipeTracking = true;
			openSwipeLocked = false;
		}
	}}
	ontouchmove={(e) => {
		if (!openSwipeTracking) return;
		const x = e.touches[0].clientX;
		const y = e.touches[0].clientY;
		const dx = x - openSwipeStartX;
		const dy = y - openSwipeStartY;

		if (!openSwipeLocked) {
			// Wait until we have enough movement to determine direction
			if (Math.abs(dx) > 10 || Math.abs(dy) > 10) {
				// Lock in as horizontal-right swipe, or abort
				if (dx > 0 && Math.abs(dx) > Math.abs(dy) * 1.5) {
					openSwipeLocked = true;
				} else {
					openSwipeTracking = false;
					return;
				}
			} else {
				return;
			}
		}

		// Once locked, open as soon as swipe distance is reached
		if (dx >= openSwipeMinDistance) {
			uiStore.openSidebar();
			openSwipeTracking = false;
		}
	}}
	ontouchend={() => {
		openSwipeTracking = false;
	}}
/>

{#if uiStore.isMobile && uiStore.sidebarOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="backdrop" onclick={() => uiStore.closeSidebar()}></div>
{/if}

<aside
	class="sidebar"
	class:collapsed={!uiStore.sidebarOpen}
	class:mobile={uiStore.isMobile}
	bind:this={sidebarEl}
	ontouchstart={handleTouchStart}
	ontouchmove={handleTouchMove}
	ontouchend={handleTouchEnd}
>
	<div class="sidebar-inner">
		<div class="sidebar-header">
			<WorkspaceSwitcher />
		</div>

		{#if wsSlug}
			<nav class="collection-nav">
				<a
					href="/{wsSlug}"
					class="nav-item dashboard"
					class:active={isDashboardPage}
					onclick={() => uiStore.onNavigate()}
				>
					<span class="nav-icon">📊</span>
					<span class="nav-label">Dashboard</span>
				</a>

				{#if sidebarCollections.length > 0}
					<div
						class="nav-section"
						use:dndzone={{items: sidebarCollections, flipDurationMs, type: 'sidebar-collection', dragDisabled: uiStore.isTouch}}
						onconsider={handleCollectionConsider}
						onfinalize={handleCollectionFinalize}
					>
						{#each sidebarCollections as collection (collection.id)}
							<a
								href="/{wsSlug}/{collection.slug}"
								class="nav-item"
								class:active={activeCollectionSlug === collection.slug}
								onclick={() => uiStore.onNavigate()}
							>
								<span class="nav-icon">{collection.icon}</span>
								<span class="nav-label">{collection.name}</span>
								{#if collection.item_count != null}
									<span class="nav-count">{collection.item_count}</span>
								{/if}
							</a>
						{/each}
					</div>
				{/if}

				{#if agentCollections.length > 0}
					<div class="nav-spacer"></div>
					{#each agentCollections as collection (collection.id)}
						<a
							href="/{wsSlug}/{collection.slug}"
							class="nav-item"
							class:active={activeCollectionSlug === collection.slug}
							onclick={() => uiStore.onNavigate()}
						>
							<span class="nav-icon">{collection.icon}</span>
							<span class="nav-label">{collection.name}</span>
							{#if collection.item_count != null}
								<span class="nav-count">{collection.item_count}</span>
							{/if}
						</a>
					{/each}
				{/if}
			</nav>

			{#if !agentSlugs.includes(activeCollectionSlug ?? '')}
			<div class="actions">
				<a
					href="/{wsSlug}/new{activeCollectionSlug ? `?collection=${activeCollectionSlug}` : ''}"
					class="new-item-btn"
					onclick={() => uiStore.onNavigate()}
				>
					+ New {activeColl?.name ? activeColl.name.replace(/s$/, '') : 'Item'}
				</a>
			</div>
			{/if}
		{/if}

		<div class="sidebar-footer">
			<button class="search-btn" onclick={() => { uiStore.openSearch(); uiStore.onNavigate(); }}>
				🔍 Search <kbd>⌘K</kbd>
			</button>
			{#if wsSlug}
				<a href="/{wsSlug}/library" class="settings-btn" onclick={() => uiStore.onNavigate()}>
					📚 Library
				</a>
				<a href="/{wsSlug}/settings" class="settings-btn" onclick={() => uiStore.onNavigate()}>
					⚙ Settings
				</a>
			{/if}
			<button class="theme-btn" onclick={toggleTheme}>
				{currentTheme === 'dark' ? '☀️ Light' : '🌙 Dark'}
			</button>
		</div>
	</div>
</aside>

<style>
	.sidebar {
		width: var(--sidebar-width);
		min-width: var(--sidebar-width);
		background: var(--bg-secondary);
		border-right: 1px solid var(--border);
		display: flex;
		flex-direction: column;
		height: 100vh;
		height: 100dvh;
		overflow: hidden;
		transition: transform 0.25s ease;
		transform: translateX(0);
	}
	.sidebar.collapsed {
		transform: translateX(calc(var(--sidebar-width) * -1));
		pointer-events: none;
	}
	.sidebar.mobile {
		position: fixed;
		z-index: 30;
		left: 0;
		top: 0;
		box-shadow: 4px 0 24px rgba(0, 0, 0, 0.3);
	}
	.sidebar.mobile.collapsed {
		box-shadow: none;
	}
	.backdrop {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.4);
		z-index: 25;
	}
	.sidebar-inner {
		display: flex;
		flex-direction: column;
		height: 100%;
		padding: var(--space-3);
		gap: var(--space-3);
		overflow: hidden;
		min-height: 0;
	}
	.sidebar-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		flex-shrink: 0;
	}
	.sidebar-header :global(.workspace-switcher) {
		flex: 1;
	}

	/* Collection navigation */
	.collection-nav {
		display: flex;
		flex-direction: column;
		gap: 2px;
		overflow-y: auto;
		flex: 1;
		min-height: 0;
	}
	.nav-spacer {
		height: 1px;
		background: var(--border);
		margin: var(--space-2) var(--space-3);
		opacity: 0.5;
	}
	.nav-section {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.nav-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.875em;
		text-decoration: none;
		transition: background 0.15s ease, color 0.15s ease;
	}
	.nav-section .nav-item {
		cursor: grab;
	}
	.nav-section .nav-item:active {
		cursor: grabbing;
	}
	.nav-item:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
		text-decoration: none;
	}
	.nav-item.active {
		background: color-mix(in srgb, var(--accent-blue) 20%, transparent);
		color: var(--accent-blue);
	}
	.nav-item.dashboard {
		margin-bottom: var(--space-2);
	}
	.nav-icon {
		flex-shrink: 0;
		width: 1.25em;
		text-align: center;
	}
	.nav-label {
		flex: 1;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.nav-count {
		flex-shrink: 0;
		font-size: 0.8em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 1px 6px;
		border-radius: 10px;
		min-width: 1.5em;
		text-align: center;
	}
	.nav-item.active .nav-count {
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		color: var(--accent-blue);
	}
	/* Actions */
	.actions {
		flex-shrink: 0;
	}
	.new-item-btn {
		display: block;
		width: 100%;
		padding: var(--space-2);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		text-align: center;
		text-decoration: none;
	}
	.new-item-btn:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
		text-decoration: none;
	}

	/* Footer */
	.sidebar-footer {
		flex-shrink: 0;
		border-top: 1px solid var(--border);
		padding-top: var(--space-3);
	}
	.search-btn {
		width: 100%;
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.85em;
	}
	.search-btn:hover { background: var(--bg-hover); color: var(--text-secondary); }
	.settings-btn {
		display: block;
		width: 100%;
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.85em;
		text-decoration: none;
		margin-top: var(--space-2);
	}
	.settings-btn:hover { background: var(--bg-hover); color: var(--text-secondary); text-decoration: none; }
	.theme-btn {
		width: 100%;
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.85em;
		margin-top: var(--space-2);
	}
	.theme-btn:hover { background: var(--bg-hover); color: var(--text-secondary); }
	kbd {
		background: var(--bg-primary);
		padding: 1px 5px;
		border-radius: 3px;
		font-size: 0.85em;
		font-family: var(--font-mono);
	}
</style>
