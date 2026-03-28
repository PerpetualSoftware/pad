<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { dndzone } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { parseSchema, parseSettings, itemUrlId } from '$lib/types';
	import type { Collection } from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';
	import WorkspaceSwitcher from './WorkspaceSwitcher.svelte';
	import NotificationPanel from '$lib/components/common/NotificationPanel.svelte';

	let notificationPanelOpen = $state(false);

	let wsSlug = $derived(workspaceStore.current?.slug);
	let isDashboardPage = $derived(wsSlug ? page.url.pathname === `/${wsSlug}` : false);
	let isActivityPage = $derived(wsSlug ? page.url.pathname === `/${wsSlug}/activity` : false);

	let activeCollectionSlug = $derived.by(() => {
		if (!wsSlug) return null;
		const prefix = `/${wsSlug}/`;
		const path = page.url.pathname;
		if (!path.startsWith(prefix)) return null;
		const rest = path.slice(prefix.length);
		const slug = rest.split('/')[0];
		if (slug === 'settings' || slug === 'new' || slug === 'library' || slug === 'activity' || slug === '') return null;
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

	async function createNewItem() {
		if (!wsSlug || !activeCollectionSlug) return;
		const coll = activeColl;
		if (!coll) return;
		try {
			const schema = parseSchema(coll);
			const settings = parseSettings(coll);
			const defaultFields: Record<string, any> = {};
			const statusField = schema.fields.find(f => f.key === 'status');
			if (statusField?.options?.length) {
				defaultFields.status = statusField.options[0];
			}
			const item = await api.items.create(wsSlug, activeCollectionSlug, {
				title: 'Untitled',
				content: settings.content_template || '',
				fields: JSON.stringify(defaultFields),
				source: 'web'
			});
			uiStore.onNavigate();
			goto(`/${wsSlug}/${activeCollectionSlug}/${itemUrlId(item)}?new=1`);
		} catch {
			toastStore.show('Failed to create item', 'error');
		}
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

<!-- Swipe from left edge to open (when sidebar is closed on mobile) -->
<!-- Touch zone: 0-24px from left edge only — avoids conflicts with board horizontal scroll -->
<svelte:window
	ontouchstart={(e) => {
		if (!uiStore.isMobile || uiStore.sidebarOpen) return;
		const x = e.touches[0].clientX;
		if (x <= 24) {
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
				<a
					href="/{wsSlug}/activity"
					class="nav-item"
					class:active={isActivityPage}
					onclick={() => uiStore.onNavigate()}
				>
					<span class="nav-icon">📋</span>
					<span class="nav-label">Activity</span>
				</a>

				{#if sidebarCollections.length > 0}
					<div class="section-header">
						<span class="section-label">Collections</span>
					</div>
					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						class="nav-section"
						use:dndzone={{items: sidebarCollections, flipDurationMs, type: 'sidebar-collection', dragDisabled: uiStore.isTouch}}
						onconsider={handleCollectionConsider}
						onfinalize={handleCollectionFinalize}
					>
						{#each sidebarCollections as collection (collection.id)}
							<a
								href="/{wsSlug}/{collection.slug}"
								class="nav-item draggable"
								class:active={activeCollectionSlug === collection.slug}
								onclick={() => uiStore.onNavigate()}
							>
								<span class="drag-handle" title="Drag to reorder">⠿</span>
								<span class="nav-icon">{collection.icon}</span>
								<span class="nav-label">{collection.name}</span>
								{#if collection.active_item_count != null && collection.active_item_count > 0}
									<span class="nav-count">{collection.active_item_count}</span>
								{:else if collection.active_item_count == null && collection.item_count != null && collection.item_count > 0}
									<span class="nav-count">{collection.item_count}</span>
								{/if}
							</a>
						{/each}
					</div>
				{/if}

				{#if agentCollections.length > 0}
					<div class="section-header agent-section">
						<span class="section-label">Agent</span>
					</div>
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

			{#if !agentSlugs.includes(activeCollectionSlug ?? '') && activeCollectionSlug}
			<div class="actions">
				<button
					class="new-item-btn"
					onclick={createNewItem}
				>
					+ New {activeColl?.name ? activeColl.name.replace(/s$/, '') : 'Item'}
				</button>
			</div>
			{/if}
		{/if}

		<div class="sidebar-footer">
			<button class="search-btn" onclick={() => { uiStore.openSearch(); uiStore.onNavigate(); }}>
				🔍 Search <kbd>⌘K</kbd>
			</button>
			{#if wsSlug}
				<a href="/{wsSlug}/settings" class="settings-btn" onclick={() => uiStore.onNavigate()}>
					⚙ Settings
				</a>
			{/if}
			<div class="footer-row">
				<button class="theme-btn" onclick={toggleTheme}>
					{currentTheme === 'dark' ? '☀️ Light' : '🌙 Dark'}
				</button>
				<button
					class="bell-btn"
					onclick={() => { notificationPanelOpen = !notificationPanelOpen; }}
					aria-label="Notification history"
				>
					<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
						<path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"/>
						<path d="M13.73 21a2 2 0 0 1-3.46 0"/>
					</svg>
					{#if toastStore.unreadCount > 0}
						<span class="bell-badge">{toastStore.unreadCount > 9 ? '9+' : toastStore.unreadCount}</span>
					{/if}
				</button>
			</div>
		</div>
	</div>
</aside>

<NotificationPanel visible={notificationPanelOpen} onclose={() => { notificationPanelOpen = false; }} />

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
	.section-header {
		padding: var(--space-3) var(--space-3) var(--space-1);
	}
	.section-header.agent-section {
		margin-top: var(--space-2);
		border-top: 1px solid var(--border);
		padding-top: var(--space-3);
	}
	.section-label {
		font-size: 0.7em;
		font-weight: 600;
		color: var(--text-muted);
		text-transform: uppercase;
		letter-spacing: 0.06em;
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
	.nav-item.draggable {
		cursor: grab;
	}
	.nav-item.draggable:active {
		cursor: grabbing;
	}
	.drag-handle {
		flex-shrink: 0;
		width: 1em;
		text-align: center;
		color: var(--text-muted);
		font-size: 0.75em;
		opacity: 0;
		transition: opacity 0.15s;
		cursor: grab;
		user-select: none;
		margin-left: -4px;
		margin-right: -4px;
	}
	.nav-item.draggable:hover .drag-handle {
		opacity: 0.5;
	}
	.nav-item.draggable:active .drag-handle {
		opacity: 1;
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
	.footer-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-top: var(--space-2);
	}
	.theme-btn {
		flex: 1;
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.85em;
	}
	.theme-btn:hover { background: var(--bg-hover); color: var(--text-secondary); }
	.bell-btn {
		position: relative;
		display: flex;
		align-items: center;
		justify-content: center;
		width: 32px;
		height: 32px;
		flex-shrink: 0;
		border-radius: var(--radius);
		color: var(--text-muted);
		background: none;
		border: none;
		cursor: pointer;
	}
	.bell-btn:hover {
		background: var(--bg-hover);
		color: var(--text-secondary);
	}
	.bell-badge {
		position: absolute;
		top: 2px;
		right: 2px;
		min-width: 16px;
		height: 16px;
		padding: 0 4px;
		background: var(--accent-red, #ef4444);
		color: #fff;
		font-size: 0.65em;
		font-weight: 700;
		line-height: 16px;
		text-align: center;
		border-radius: 8px;
		pointer-events: none;
	}
	kbd {
		background: var(--bg-primary);
		padding: 1px 5px;
		border-radius: 3px;
		font-size: 0.85em;
		font-family: var(--font-mono);
	}
</style>
