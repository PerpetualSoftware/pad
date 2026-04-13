<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { dndzone } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client';
	import { parseSchema, parseSettings, itemUrlId } from '$lib/types';
	import type { Collection } from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';
	import NotificationPanel from '$lib/components/common/NotificationPanel.svelte';
	import CreateCollectionModal from '$lib/components/collections/CreateCollectionModal.svelte';

	let notificationPanelOpen = $state(false);
	let showCreateCollection = $state(false);
	let quickAddCollection = $state<Collection | null>(null);
	let quickAddTitle = $state('');
	let quickAddInputEl = $state<HTMLTextAreaElement>();

	let wsSlug = $derived(workspaceStore.current?.slug);
	let wsUsername = $derived(workspaceStore.current?.owner_username ?? '');
	let wsPrefix = $derived(wsUsername && wsSlug ? `/${wsUsername}/${wsSlug}` : '');
	let isGuest = $derived(workspaceStore.current?.is_guest ?? false);
	let isDashboardPage = $derived(wsPrefix ? page.url.pathname === wsPrefix : false);
	let isRolesPage = $derived(wsPrefix ? page.url.pathname === `${wsPrefix}/roles` : false);
	let isActivityPage = $derived(wsPrefix ? page.url.pathname === `${wsPrefix}/activity` : false);

	let activeCollectionSlug = $derived.by(() => {
		if (!wsPrefix) return null;
		const prefix = `${wsPrefix}/`;
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

	function startQuickAdd(coll: Collection) {
		quickAddCollection = coll;
		quickAddTitle = '';
	}

	// Watch for Cmd-N quick-add requests from the layout
	$effect(() => {
		if (uiStore.quickAddRequested) {
			uiStore.clearQuickAddRequest();
			const target = activeColl
				?? regularCollections.find(c => c.slug === 'tasks')
				?? regularCollections[0];
			if (target) startQuickAdd(target);
		}
	});

	function autofocus(node: HTMLElement) {
		requestAnimationFrame(() => node.focus());
	}

	function cancelQuickAdd() {
		quickAddCollection = null;
		quickAddTitle = '';
	}

	async function submitQuickAdd() {
		if (!wsSlug || !quickAddCollection || !quickAddTitle.trim()) return;
		const coll = quickAddCollection;
		const title = quickAddTitle.trim();
		cancelQuickAdd();
		try {
			const schema = parseSchema(coll);
			const settings = parseSettings(coll);
			const defaultFields: Record<string, any> = {};
			const statusField = schema.fields.find(f => f.key === 'status');
			if (statusField?.options?.length) {
				defaultFields.status = statusField.options[0];
			}
			const item = await api.items.create(wsSlug, coll.slug, {
				title,
				content: settings.content_template || '',
				fields: JSON.stringify(defaultFields),
				source: 'web'
			});
			uiStore.onNavigate();
			goto(`${wsPrefix}/${coll.slug}/${itemUrlId(item)}?new=1`);
		} catch (err: any) {
			toastStore.show(err?.message || 'Failed to create item', 'error');
		}
	}

	function handleQuickAddKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter') {
			e.preventDefault();
			submitQuickAdd();
		} else if (e.key === 'Escape') {
			cancelQuickAdd();
		}
	}

	let versionLabel = $state('');

	async function handleLogout() {
		try {
			await api.auth.logout();
			window.location.href = '/login';
		} catch {}
	}

	let currentTheme = $state<'dark' | 'light'>('dark');

	function toggleTheme() {
		currentTheme = currentTheme === 'dark' ? 'light' : 'dark';
		document.documentElement.setAttribute('data-theme', currentTheme);
		localStorage.setItem('pad-theme', currentTheme);
	}

	onMount(async () => {
		const saved = localStorage.getItem('pad-theme');
		if (saved === 'light' || saved === 'dark') {
			currentTheme = saved;
		} else if (window.matchMedia('(prefers-color-scheme: light)').matches) {
			currentTheme = 'light';
		}

		try {
			const health = await api.health();
			if (health.version) {
				const v = health.version === 'dev' ? 'dev' : `v${health.version}`;
				versionLabel = health.commit ? `${v} (${health.commit.slice(0, 7)})` : v;
			}
		} catch {}
	});

	function handleTouchStart(e: TouchEvent) {
		// Don't initiate swipe-to-close if the touch started inside a
		// horizontally scrollable element (e.g. the mobile workspace list)
		let el = e.target as HTMLElement | null;
		while (el && el !== sidebarEl) {
			if (el.scrollWidth > el.clientWidth + 1) return;
			el = el.parentElement;
		}
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
<!-- Touch zone: 0-16px from left edge only — skip if touch is inside a horizontally scrollable element -->
<svelte:window
	ontouchstart={(e) => {
		if (!uiStore.isMobile || uiStore.sidebarOpen) return;
		const x = e.touches[0].clientX;
		if (x <= 16) {
			// Don't activate if the touch is inside a horizontally scrollable container
			// (e.g. board view with overflow-x: auto)
			let el = e.target as HTMLElement | null;
			while (el) {
				if (el.scrollWidth > el.clientWidth + 1) return;
				el = el.parentElement;
			}
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
		{#if wsSlug}
			<nav class="collection-nav">
				{#if !isGuest}
				<a
					href="{wsPrefix}"
					class="nav-item dashboard"
					class:active={isDashboardPage}
					onclick={() => uiStore.onNavigate()}
				>
					<span class="nav-icon">📊</span>
					<span class="nav-label">Dashboard</span>
				</a>
				<a
					href="{wsPrefix}/roles"
					class="nav-item"
					class:active={isRolesPage}
					onclick={() => uiStore.onNavigate()}
				>
					<span class="nav-icon">🎭</span>
					<span class="nav-label">Roles</span>
				</a>
				<a
					href="{wsPrefix}/activity"
					class="nav-item"
					class:active={isActivityPage}
					onclick={() => uiStore.onNavigate()}
				>
					<span class="nav-icon">📋</span>
					<span class="nav-label">Activity</span>
				</a>
				{/if}

				{#if isGuest}
					<div class="section-header">
						<span class="section-label">Shared with you</span>
					</div>
				{/if}

				{#if sidebarCollections.length > 0}
					{#if !isGuest}
					<div class="section-header">
						<span class="section-label">Collections</span>
						<button
							class="section-add-btn"
							type="button"
							onclick={() => { showCreateCollection = true; }}
							title="New collection"
						>+</button>
					</div>
					{/if}
					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						class="nav-section"
						use:dndzone={{items: sidebarCollections, flipDurationMs, type: 'sidebar-collection', dragDisabled: uiStore.isTouch}}
						onconsider={handleCollectionConsider}
						onfinalize={handleCollectionFinalize}
					>
						{#each sidebarCollections as collection (collection.id)}
							<a
								href="{wsPrefix}/{collection.slug}"
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
								<button
									class="nav-quick-add"
									title="New {collection.name.replace(/s$/, '')}"
									onclick={(e) => { e.stopPropagation(); e.preventDefault(); startQuickAdd(collection); }}
								>+</button>
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
							href="{wsPrefix}/{collection.slug}"
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

			{#if !agentSlugs.includes(activeCollectionSlug ?? '') && activeCollectionSlug && activeColl}
			<div class="actions">
				<button
					class="new-item-btn"
					onclick={() => startQuickAdd(activeColl)}
				>
					+ New {activeColl.name ? activeColl.name.replace(/s$/, '') : 'Item'}
				</button>
			</div>
			{/if}
		{/if}

		<div class="sidebar-footer">
			<button class="search-btn" onclick={() => { uiStore.openSearch(); uiStore.onNavigate(); }}>
				🔍 Search <kbd>⌘K</kbd>
			</button>
			<div class="footer-row">
				{#if wsSlug && !isGuest}
					<a href="{wsPrefix}/settings" class="settings-btn" onclick={() => uiStore.onNavigate()}>
						⚙ Settings
					</a>
				{/if}
				{#if !uiStore.isMobile}
					<button
						class="collapse-sidebar-btn"
						onclick={() => uiStore.closeSidebar()}
						title="Hide sidebar (⌘\)"
						aria-label="Hide sidebar"
					>
						<svg width="16" height="16" viewBox="0 0 16 16" fill="none">
							<path d="M11 3L6 8L11 13" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
						</svg>
					</button>
				{/if}
				<button
					class="theme-btn"
					onclick={toggleTheme}
					title="{currentTheme === 'dark' ? 'Light' : 'Dark'} mode"
					aria-label="Toggle theme"
				>
					{#if currentTheme === 'dark'}
						<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
							<circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/>
						</svg>
					{:else}
						<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
							<path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/>
						</svg>
					{/if}
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
			{#if uiStore.isMobile && authStore.user}
				<div class="mobile-user-row">
					<span class="mobile-user-name">{authStore.user.name}</span>
					<button class="mobile-logout-btn" onclick={handleLogout}>Sign out</button>
				</div>
			{/if}
			{#if versionLabel}
				<span class="version-label">{versionLabel}</span>
			{/if}
		</div>
	</div>
</aside>

{#if wsSlug}
	<CreateCollectionModal
		open={showCreateCollection}
		{wsSlug}
		oncreated={() => {
			showCreateCollection = false;
			if (wsSlug) collectionStore.loadCollections(wsSlug);
		}}
		onclose={() => { showCreateCollection = false; }}
	/>
{/if}

{#if quickAddCollection}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div class="quick-add-overlay" onclick={cancelQuickAdd}>
		<div class="quick-add-modal" onclick={(e) => e.stopPropagation()}>
			<div class="quick-add-header">
				<span class="quick-add-icon">{quickAddCollection.icon}</span>
				<span class="quick-add-label">New {quickAddCollection.name.replace(/s$/, '')}</span>
			</div>
			<textarea
				class="quick-add-input"
				rows="1"
				placeholder="What's the title?"
				use:autofocus
				bind:this={quickAddInputEl}
				bind:value={quickAddTitle}
				onkeydown={handleQuickAddKeydown}
				oninput={(e) => { const el = e.currentTarget; el.style.height = 'auto'; el.style.height = el.scrollHeight + 'px'; }}
			></textarea>
			<div class="quick-add-actions">
				<span class="quick-add-hint">Enter to create · Esc to cancel</span>
				<button
					class="quick-add-btn"
					type="button"
					disabled={!quickAddTitle.trim()}
					onclick={submitQuickAdd}
				>Create</button>
			</div>
		</div>
	</div>
{/if}

<NotificationPanel visible={notificationPanelOpen} onclose={() => { notificationPanelOpen = false; }} />

<style>
	.sidebar {
		width: var(--sidebar-width);
		min-width: var(--sidebar-width);
		background: var(--bg-secondary);
		border-right: 1px solid var(--border);
		display: flex;
		flex-direction: column;
		height: 100%;
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
		top: var(--topbar-height);
		height: calc(100vh - var(--topbar-height));
		height: calc(100dvh - var(--topbar-height));
		box-shadow: 4px 0 24px rgba(0, 0, 0, 0.3);
	}
	.sidebar.mobile.collapsed {
		box-shadow: none;
	}
	.backdrop {
		position: fixed;
		top: var(--topbar-height);
		left: 0;
		right: 0;
		bottom: 0;
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
		display: flex;
		align-items: center;
		justify-content: space-between;
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
	.section-add-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.85em;
		cursor: pointer;
		padding: 0 var(--space-1);
		border-radius: var(--radius-sm);
		line-height: 1;
		opacity: 0;
		transition: opacity 0.15s, color 0.15s;
	}
	.section-header:hover .section-add-btn {
		opacity: 1;
	}
	@media (hover: none) {
		.section-add-btn {
			opacity: 1;
		}
	}
	.section-add-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}
	.nav-quick-add {
		flex-shrink: 0;
		width: 1.4em;
		height: 1.4em;
		display: flex;
		align-items: center;
		justify-content: center;
		border: none;
		background: none;
		color: var(--text-muted);
		font-size: 0.8em;
		font-weight: 600;
		line-height: 1;
		border-radius: var(--radius-sm);
		cursor: pointer;
		padding: 0;
		visibility: hidden;
	}
	.nav-item:hover .nav-quick-add {
		visibility: visible;
	}
	@media (hover: none) {
		.nav-quick-add {
			visibility: visible;
		}
	}
	.nav-quick-add:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
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
	.quick-add-overlay {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 50;
		display: flex;
		justify-content: center;
		align-items: flex-start;
		padding-top: 20vh;
	}
	.quick-add-modal {
		width: 100%;
		max-width: 560px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 60px rgba(0, 0, 0, 0.5);
		padding: var(--space-4) var(--space-5);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.quick-add-header {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.quick-add-icon {
		font-size: 1.1em;
	}
	.quick-add-label {
		font-size: 0.9em;
		font-weight: 600;
		color: var(--text-secondary);
	}
	.quick-add-input {
		width: 100%;
		padding: var(--space-3);
		background: var(--bg-tertiary);
		border: 1px solid transparent;
		border-radius: var(--radius);
		font-size: 1em;
		color: var(--text-primary);
		outline: none;
		resize: none;
		overflow: hidden;
		line-height: 1.4;
		font-family: inherit;
	}
	.quick-add-input:focus {
		border-color: var(--accent-blue);
	}
	.quick-add-input::placeholder {
		color: var(--text-muted);
	}
	.quick-add-actions {
		display: flex;
		align-items: center;
		justify-content: space-between;
	}
	.quick-add-hint {
		font-size: 0.75em;
		color: var(--text-muted);
	}
	.quick-add-btn {
		padding: var(--space-2) var(--space-4);
		background: var(--accent-blue);
		border: none;
		border-radius: var(--radius);
		color: #fff;
		font-size: 0.85em;
		font-weight: 500;
		cursor: pointer;
	}
	.quick-add-btn:hover:not(:disabled) {
		filter: brightness(1.1);
	}
	.quick-add-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
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
	.footer-row {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-top: var(--space-2);
	}
	.settings-btn {
		flex: 1;
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.85em;
		text-decoration: none;
	}
	.settings-btn:hover { background: var(--bg-hover); color: var(--text-secondary); text-decoration: none; }
	.version-label {
		display: block;
		text-align: center;
		font-size: 0.7em;
		color: var(--text-muted);
		opacity: 0.6;
		margin-top: var(--space-2);
		user-select: text;
	}
	.theme-btn {
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
	.theme-btn:hover {
		background: var(--bg-hover);
		color: var(--text-secondary);
	}
	.collapse-sidebar-btn {
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
	.collapse-sidebar-btn:hover {
		background: var(--bg-hover);
		color: var(--text-secondary);
	}
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
	/* Mobile sign-out row */
	.mobile-user-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-2) var(--space-3);
		margin-top: var(--space-2);
		border-top: 1px solid var(--border);
		font-size: 0.8em;
		color: var(--text-muted);
	}
	.mobile-user-name {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.mobile-logout-btn {
		color: var(--text-muted);
		font-size: 0.85em;
		flex-shrink: 0;
	}
	.mobile-logout-btn:hover {
		color: var(--text-secondary);
	}
	kbd {
		background: var(--bg-primary);
		padding: 1px 5px;
		border-radius: 3px;
		font-size: 0.85em;
		font-family: var(--font-mono);
	}
</style>
