<script lang="ts">
	import { dndzone } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { api } from '$lib/api/client';
	import type { Workspace } from '$lib/types';
	import { onMount } from 'svelte';
	import PadLogo from '$lib/components/layout/PadLogo.svelte';

	let { mobile = false }: { mobile?: boolean } = $props();

	let userMenuOpen = $state(false);
	let currentTheme = $state<'dark' | 'light'>('dark');

	let currentSlug = $derived(workspaceStore.current?.slug ?? '');
	let currentUsername = $derived(workspaceStore.current?.owner_username ?? '');

	// DnD state — local copy of workspaces for reordering
	let dndWorkspaces: Workspace[] = $state([]);
	let isDragging = $state(false);
	const flipDurationMs = 150;

	// Mobile edit mode
	let mobileEditMode = $state(false);

	// Sync from store when not actively reordering
	$effect(() => {
		if (!isDragging && !mobileEditMode) {
			dndWorkspaces = [...workspaceStore.workspaces];
		}
	});

	// Desktop: svelte-dnd-action handlers
	function handleConsider(e: CustomEvent<DndEvent<Workspace>>) {
		dndWorkspaces = e.detail.items;
		isDragging = true;
	}

	async function handleFinalize(e: CustomEvent<DndEvent<Workspace>>) {
		dndWorkspaces = e.detail.items;
		isDragging = false;
		await saveOrder();
	}

	// Mobile edit mode: svelte-dnd-action handlers (touch drag works fine
	// when items are buttons, not links)
	function handleMobileConsider(e: CustomEvent<DndEvent<Workspace>>) {
		dndWorkspaces = e.detail.items;
	}

	async function handleMobileFinalize(e: CustomEvent<DndEvent<Workspace>>) {
		dndWorkspaces = e.detail.items;
	}

	async function saveOrder() {
		const updates = dndWorkspaces.map((ws, i) => ({
			slug: ws.slug,
			sort_order: i
		}));
		try {
			await api.workspaces.reorder(updates);
			await workspaceStore.loadAll();
		} catch {
			dndWorkspaces = [...workspaceStore.workspaces];
		}
	}

	function enterEditMode() {
		mobileEditMode = true;
		dndWorkspaces = [...workspaceStore.workspaces];
	}

	async function exitEditMode() {
		mobileEditMode = false;
		await saveOrder();
	}

	// Color palette for workspace circles
	const colors = [
		'#4a9eff', '#4ade80', '#a78bfa', '#fbbf24',
		'#22d3ee', '#fb923c', '#f472b6', '#34d399',
	];

	function wsColor(name: string): string {
		let hash = 0;
		for (let i = 0; i < name.length; i++) {
			hash = name.charCodeAt(i) + ((hash << 5) - hash);
		}
		return colors[Math.abs(hash) % colors.length];
	}

	function wsInitial(name: string): string {
		return name.charAt(0).toUpperCase();
	}

	onMount(() => {
		const saved = localStorage.getItem('pad-theme');
		if (saved === 'light' || saved === 'dark') {
			currentTheme = saved;
		} else if (window.matchMedia('(prefers-color-scheme: light)').matches) {
			currentTheme = 'light';
		}
	});

	function toggleTheme() {
		currentTheme = currentTheme === 'dark' ? 'light' : 'dark';
		document.documentElement.setAttribute('data-theme', currentTheme);
		localStorage.setItem('pad-theme', currentTheme);
	}

	async function handleLogout() {
		try {
			await api.auth.logout();
			window.location.href = '/login';
		} catch {}
	}

	function closeUserMenu() {
		userMenuOpen = false;
	}
</script>

<svelte:window onclick={(e) => {
	if (userMenuOpen) {
		const target = e.target as HTMLElement;
		if (!target.closest('.user-menu-container')) {
			closeUserMenu();
		}
	}
}} />

{#if !mobile}
	<!-- ── Desktop ────────────────────────────────────────────────────────── -->
	<header class="topbar">
		<div class="topbar-left">
			<PadLogo />
		</div>
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div
			class="workspace-list"
			use:dndzone={{items: dndWorkspaces, flipDurationMs, type: 'topbar-workspace', dragDisabled: uiStore.isTouch}}
			onconsider={handleConsider}
			onfinalize={handleFinalize}
		>
			{#each dndWorkspaces as ws (ws.id)}
				<a
					href="/{ws.owner_username}/{ws.slug}"
					class="workspace-item"
					class:active={ws.slug === currentSlug}
					title={ws.name}
				>
					<span
						class="workspace-icon"
						style="background: {ws.slug === currentSlug ? wsColor(ws.name) : 'transparent'}; color: {ws.slug === currentSlug ? '#fff' : 'var(--text-secondary)'}; border-color: {wsColor(ws.name)}"
					>
						{wsInitial(ws.name)}
					</span>
					<span class="workspace-name">{ws.name}</span>
				</a>
			{/each}
		</div>
		<button
			class="workspace-add"
			onclick={() => uiStore.openCreateWorkspace()}
			title="New workspace"
		>
			<span class="add-icon">+</span>
		</button>

		<div class="topbar-right">
			<button
				class="collapse-btn"
				onclick={() => uiStore.closeTopbar()}
				title="Hide workspace bar (⌘\)"
				aria-label="Hide workspace bar"
			>
				<svg width="14" height="14" viewBox="0 0 16 16" fill="none">
					<path d="M3 11L8 6L13 11" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/>
				</svg>
			</button>
			{#if authStore.user}
				<div class="user-menu-container">
					<button
						class="user-trigger"
						onclick={() => userMenuOpen = !userMenuOpen}
					>
						<span class="user-avatar" style="background: {wsColor(authStore.user.name || authStore.user.email)}">
							{(authStore.user.name || authStore.user.email).charAt(0).toUpperCase()}
						</span>
					</button>

					{#if userMenuOpen}
						<div class="user-dropdown">
							<div class="user-info">
								<span class="user-dropdown-name">{authStore.user.name}</span>
								<span class="user-dropdown-email">{authStore.user.email}</span>
							</div>
							<div class="dropdown-divider"></div>
							<a href="/console" class="dropdown-item" onclick={closeUserMenu}>
								Workspaces
							</a>
							<a href="/console/settings" class="dropdown-item" onclick={closeUserMenu}>
								Settings
							</a>
							{#if authStore.cloudMode}
								<a href="/console/billing" class="dropdown-item" onclick={closeUserMenu}>
									Billing
								</a>
							{/if}
							{#if authStore.user?.role === 'admin'}
								<a href="/console/admin" class="dropdown-item" onclick={closeUserMenu}>
									Admin
								</a>
							{/if}
							<button class="dropdown-item" onclick={toggleTheme}>
								{currentTheme === 'dark' ? 'Light mode' : 'Dark mode'}
							</button>
							<div class="dropdown-divider"></div>
							<button class="dropdown-item logout" onclick={handleLogout}>
								Sign out
							</button>
						</div>
					{/if}
				</div>
			{/if}
		</div>
	</header>
{:else}
	<!-- ── Mobile ─────────────────────────────────────────────────────────── -->
	<header class="topbar topbar-mobile">
		<div class="topbar-left">
			<PadLogo />
		</div>
		<div class="workspace-list">
			{#each workspaceStore.workspaces as ws (ws.id)}
				<a
					href="/{ws.owner_username}/{ws.slug}"
					class="workspace-item"
					class:active={ws.slug === currentSlug}
					onclick={() => uiStore.onNavigate()}
				>
					<span
						class="workspace-icon"
						style="background: {ws.slug === currentSlug ? wsColor(ws.name) : 'transparent'}; color: {ws.slug === currentSlug ? '#fff' : 'var(--text-secondary)'}; border-color: {wsColor(ws.name)}"
					>
						{wsInitial(ws.name)}
					</span>
					<span class="workspace-name">{ws.name}</span>
				</a>
			{/each}
		</div>
		<button
			class="workspace-add"
			onclick={() => { uiStore.onNavigate(); uiStore.openCreateWorkspace(); }}
			title="New workspace"
		>
			<span class="add-icon">+</span>
		</button>
		{#if workspaceStore.workspaces.length > 1}
			<button
				class="edit-btn"
				onclick={enterEditMode}
				title="Reorder workspaces"
			>
				<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round">
					<line x1="4" y1="9" x2="20" y2="9" /><line x1="4" y1="15" x2="20" y2="15" />
					<polyline points="8 5 4 9 8 13" /><polyline points="16 11 20 15 16 19" />
				</svg>
			</button>
		{/if}
		{#if authStore.user}
			<div class="user-menu-container">
				<button
					class="user-trigger"
					onclick={() => userMenuOpen = !userMenuOpen}
				>
					<span class="user-avatar" style="background: {wsColor(authStore.user.name || authStore.user.email)}">
						{(authStore.user.name || authStore.user.email).charAt(0).toUpperCase()}
					</span>
				</button>

				{#if userMenuOpen}
					<div class="user-dropdown">
						<div class="user-info">
							<span class="user-dropdown-name">{authStore.user.name}</span>
							<span class="user-dropdown-email">{authStore.user.email}</span>
						</div>
						<div class="dropdown-divider"></div>
						<a href="/console" class="dropdown-item" onclick={closeUserMenu}>
							Workspaces
						</a>
						<a href="/console/settings" class="dropdown-item" onclick={closeUserMenu}>
							Settings
						</a>
						{#if authStore.cloudMode}
							<a href="/console/billing" class="dropdown-item" onclick={closeUserMenu}>
								Billing
							</a>
						{/if}
						{#if authStore.user?.role === 'admin'}
							<a href="/console/admin" class="dropdown-item" onclick={closeUserMenu}>
								Admin
							</a>
						{/if}
						<button class="dropdown-item" onclick={toggleTheme}>
							{currentTheme === 'dark' ? 'Light mode' : 'Dark mode'}
						</button>
						<div class="dropdown-divider"></div>
						<button class="dropdown-item logout" onclick={handleLogout}>
							Sign out
						</button>
					</div>
				{/if}
			</div>
		{/if}
	</header>

	<!-- Full-screen reorder overlay -->
	{#if mobileEditMode}
		<div class="reorder-overlay">
			<div class="reorder-header">
				<h3 class="reorder-title">Reorder Workspaces</h3>
				<button class="done-btn" onclick={exitEditMode}>Done</button>
			</div>
			<!-- svelte-ignore a11y_no_static_element_interactions -->
			<div
				class="reorder-list"
				use:dndzone={{items: dndWorkspaces, flipDurationMs, type: 'mobile-reorder'}}
				onconsider={handleMobileConsider}
				onfinalize={handleMobileFinalize}
			>
				{#each dndWorkspaces as ws (ws.id)}
					<div class="reorder-item" class:active={ws.slug === currentSlug}>
						<span class="drag-grip">⠿</span>
						<span
							class="reorder-icon"
							style="background: {wsColor(ws.name)}; border-color: {wsColor(ws.name)}"
						>
							{wsInitial(ws.name)}
						</span>
						<span class="reorder-name">{ws.name}</span>
					</div>
				{/each}
			</div>
		</div>
	{/if}
{/if}

<style>
	.topbar {
		position: relative;
		display: flex;
		align-items: center;
		justify-content: center;
		height: var(--topbar-height);
		min-height: var(--topbar-height);
		background: var(--bg-secondary);
		border-bottom: 1px solid var(--border);
		padding: 0 72px 0 56px; /* clear absolute-positioned logo (left) and user menu (right) */
		gap: var(--space-2);
		z-index: 20;
	}

	/* Mobile: fixed at top, full viewport width, above sidebar + backdrop */
	.topbar-mobile {
		position: fixed;
		top: 0;
		left: 0;
		right: 0;
		z-index: 35;
	}

	/* Workspace list — scrollable horizontally */
	.workspace-list {
		display: flex;
		align-items: center;
		gap: 2px;
		min-width: 0;
		max-width: 100%;
		overflow-x: auto;
		scrollbar-width: none;
		-ms-overflow-style: none;
		padding-right: var(--space-2);
	}
	.workspace-list::-webkit-scrollbar {
		display: none;
	}

	.workspace-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius);
		text-decoration: none;
		color: var(--text-secondary);
		white-space: nowrap;
		flex-shrink: 0;
		transition: background 0.15s, color 0.15s;
	}
	/* Grab cursor on desktop only */
	.topbar:not(.topbar-mobile) .workspace-item {
		cursor: grab;
	}
	.topbar:not(.topbar-mobile) .workspace-item:active {
		cursor: grabbing;
	}
	.workspace-item:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
		text-decoration: none;
	}
	.workspace-item.active {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.workspace-icon {
		width: 24px;
		height: 24px;
		border-radius: 50%;
		display: flex;
		align-items: center;
		justify-content: center;
		font-size: 0.75em;
		font-weight: 700;
		flex-shrink: 0;
		border: 2px solid;
		transition: background 0.15s, color 0.15s;
	}

	.workspace-name {
		font-size: 0.82em;
		font-weight: 500;
	}

	.workspace-add {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 24px;
		height: 24px;
		border-radius: 50%;
		flex-shrink: 0;
		color: var(--text-muted);
		border: 2px dashed var(--border);
		transition: border-color 0.15s, color 0.15s;
	}
	.workspace-add:hover {
		border-color: var(--text-secondary);
		color: var(--text-secondary);
	}
	.add-icon {
		font-size: 0.85em;
		font-weight: 600;
		line-height: 1;
	}

	/* Edit / Done buttons for mobile reorder */
	.edit-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		height: 28px;
		flex-shrink: 0;
		border-radius: var(--radius);
		color: var(--text-muted);
		transition: background 0.15s, color 0.15s;
	}
	.edit-btn:hover {
		background: var(--bg-hover);
		color: var(--text-secondary);
	}

	.done-btn {
		flex-shrink: 0;
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius);
		font-size: 0.78em;
		font-weight: 600;
		color: var(--accent-blue);
		background: color-mix(in srgb, var(--accent-blue) 15%, transparent);
		transition: background 0.15s;
	}
	.done-btn:hover {
		background: color-mix(in srgb, var(--accent-blue) 25%, transparent);
	}

	/* Left side — logo */
	.topbar-left {
		position: absolute;
		left: var(--space-3);
		display: flex;
		align-items: center;
		flex-shrink: 0;
		z-index: 1;
	}

	/* Right side — user menu */
	.topbar-right {
		position: absolute;
		right: var(--space-3);
		display: flex;
		align-items: center;
		gap: var(--space-1);
		flex-shrink: 0;
	}

	.collapse-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 28px;
		height: 28px;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		cursor: pointer;
		padding: 0;
		background: none;
		border: none;
		opacity: 0.5;
		transition: opacity 0.15s, color 0.15s, background 0.15s;
	}
	.collapse-btn:hover {
		opacity: 1;
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.user-menu-container {
		position: relative;
	}

	.user-trigger {
		display: flex;
		align-items: center;
		padding: 2px;
		border-radius: 50%;
		transition: opacity 0.15s;
	}
	.user-trigger:hover {
		opacity: 0.8;
	}

	.user-avatar {
		width: 28px;
		height: 28px;
		border-radius: 50%;
		display: flex;
		align-items: center;
		justify-content: center;
		font-size: 0.78em;
		font-weight: 700;
		color: #fff;
	}

	.user-dropdown {
		position: absolute;
		top: calc(100% + 6px);
		right: 0;
		min-width: 200px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 8px 24px rgba(0, 0, 0, 0.3);
		z-index: 50;
		overflow: hidden;
		animation: dropdown-in 0.12s ease-out;
	}
	@keyframes dropdown-in {
		from { opacity: 0; transform: translateY(-4px); }
		to { opacity: 1; transform: translateY(0); }
	}

	.user-info {
		padding: var(--space-3) var(--space-4);
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.user-dropdown-name {
		font-size: 0.88em;
		font-weight: 600;
		color: var(--text-primary);
	}
	.user-dropdown-email {
		font-size: 0.78em;
		color: var(--text-muted);
	}

	.dropdown-divider {
		height: 1px;
		background: var(--border);
	}

	.dropdown-item {
		display: block;
		width: 100%;
		text-align: left;
		padding: var(--space-2) var(--space-4);
		font-size: 0.85em;
		color: var(--text-secondary);
		text-decoration: none;
		transition: background 0.1s, color 0.1s;
	}
	.dropdown-item:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
		text-decoration: none;
	}
	.dropdown-item.logout {
		color: var(--accent-orange);
	}
	.dropdown-item.logout:hover {
		background: color-mix(in srgb, var(--accent-orange) 10%, var(--bg-hover));
	}

	/* ── Mobile reorder overlay ──────────────────────────────────────────── */
	.reorder-overlay {
		position: fixed;
		inset: 0;
		z-index: 40;
		background: var(--bg-primary);
		display: flex;
		flex-direction: column;
		animation: overlay-in 0.15s ease-out;
	}
	@keyframes overlay-in {
		from { opacity: 0; }
		to { opacity: 1; }
	}

	.reorder-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: var(--space-4) var(--space-4);
		border-bottom: 1px solid var(--border);
		background: var(--bg-secondary);
		flex-shrink: 0;
	}
	.reorder-title {
		font-size: 1em;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0;
	}

	.reorder-list {
		flex: 1;
		overflow-y: auto;
		padding: var(--space-3);
		display: flex;
		flex-direction: column;
		gap: 4px;
	}

	.reorder-item {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-3);
		background: var(--bg-secondary);
		border-radius: var(--radius);
		border: 1px solid var(--border);
		cursor: grab;
		touch-action: none;
		user-select: none;
		transition: box-shadow 0.15s, border-color 0.15s;
	}
	.reorder-item:active {
		cursor: grabbing;
		box-shadow: 0 4px 16px rgba(0, 0, 0, 0.25);
		border-color: var(--accent-blue);
	}
	.reorder-item.active {
		border-color: color-mix(in srgb, var(--accent-blue) 40%, var(--border));
	}

	.drag-grip {
		color: var(--text-muted);
		font-size: 1em;
		line-height: 1;
		flex-shrink: 0;
		user-select: none;
	}

	.reorder-icon {
		width: 28px;
		height: 28px;
		border-radius: 50%;
		display: flex;
		align-items: center;
		justify-content: center;
		font-size: 0.8em;
		font-weight: 700;
		color: #fff;
		flex-shrink: 0;
		border: 2px solid;
	}

	.reorder-name {
		font-size: 0.92em;
		font-weight: 500;
		color: var(--text-primary);
		flex: 1;
	}
</style>
