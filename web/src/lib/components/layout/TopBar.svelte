<script lang="ts">
	import { dndzone, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { tabsStore } from '$lib/stores/tabs.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { api } from '$lib/api/client';
	import type { WorkspaceTab } from '$lib/types';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import PadLogo from '$lib/components/layout/PadLogo.svelte';
	import WorkspaceSwitcher from '$lib/components/layout/WorkspaceSwitcher.svelte';
	import UserMenuResources from '$lib/components/layout/UserMenuResources.svelte';
	import ConnectWorkspaceModal from '$lib/components/ConnectWorkspaceModal.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import { workspaceRestoreTarget } from '$lib/utils/workspace-route';

	let { mobile = false }: { mobile?: boolean } = $props();

	let userMenuOpen = $state(false);
	let userTriggerEl: HTMLButtonElement | undefined = $state(undefined);
	let currentTheme = $state<'dark' | 'light'>('dark');
	let connectOpen = $state(false);

	let currentSlug = $derived(workspaceStore.current?.slug ?? '');

	// ── Workspace tabs (PLAN-3002 U3 / TASK-3274) ────────────────────────
	// The bar is the caller's OPEN SET from `tabsStore` (server-side, the
	// same on every device), not the workspace list. One dndzone, plain
	// horizontal scroll: the priority-plus measurement and the overflow
	// menu it fed are gone, because an open set is chosen by the user and
	// stays short, where the workspace list did not.
	//
	// dndzone needs an `id`; a tab is keyed by its slug (one row per
	// workspace), so the zone's items carry `id: slug`.
	type TabItem = WorkspaceTab & { id: string };
	const flipDurationMs = 150;
	const toItems = (tabs: WorkspaceTab[]): TabItem[] => tabs.map((t) => ({ ...t, id: t.slug }));

	let dndTabs: TabItem[] = $state([]);
	let isDragging = $state(false);
	// True from a drop until its reorder settles, so the store-to-zone sync
	// below does not snap the dropped order back to the pre-drop one while
	// the PUT is in flight.
	let persisting = $state(false);

	$effect(() => {
		const tabs = tabsStore.tabs;
		if (!isDragging && !persisting) dndTabs = toItems(tabs);
	});

	// Drag-end click suppression. After a drop the browser fires a synthetic
	// `click` on the dragged `<a>`, which svelte-dnd-action does not cancel;
	// without this guard every reorder navigates to the dragged workspace.
	let dropClickGuard = false;
	let dropClickGuardTimer: ReturnType<typeof setTimeout> | null = null;
	function armDropClickGuard() {
		dropClickGuard = true;
		if (dropClickGuardTimer) clearTimeout(dropClickGuardTimer);
		// Longer than the mouseup-to-click gap, short enough that a deliberate
		// click right after a drag is not swallowed.
		dropClickGuardTimer = setTimeout(() => {
			dropClickGuard = false;
			dropClickGuardTimer = null;
		}, 100);
	}

	function handleTabsConsider(e: CustomEvent<DndEvent<TabItem>>) {
		isDragging = true;
		dndTabs = e.detail.items;
	}

	async function handleTabsFinalize(e: CustomEvent<DndEvent<TabItem>>) {
		const items = e.detail.items.filter(
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			(t) => !(t as any)[SHADOW_ITEM_MARKER_PROPERTY_NAME]
		);
		dndTabs = items;
		isDragging = false;
		armDropClickGuard();
		const dragged = items.find((t) => t.id === e.detail.info.id);
		persisting = true;
		try {
			await tabsStore.reorder(items.map((t) => t.slug));
			// A drag keeps an ephemeral tab (PLAN-3002 Q9).
			if (dragged?.ephemeral) await tabsStore.pin(dragged.slug);
		} catch {
			// The store kept its last committed order; the sync below shows it.
		} finally {
			persisting = false;
		}
	}

	// Double-click keeps an ephemeral tab (PLAN-3002 Q9). The clicks of a
	// double-click still reach handleWsClick; its detail check stops the
	// second one from navigating again.
	function handleTabDblClick(tab: WorkspaceTab) {
		if (tab.ephemeral) void tabsStore.pin(tab.slug).catch(() => {});
	}

	// Closing a tab (PLAN-3002 Q2, Q3). Only closing the ACTIVE tab moves you:
	// to its left neighbour, or, when it was the first tab, to the tab that
	// becomes first (its right neighbour; lead ruling on TASK-3274), at that
	// tab's last route. Closing the last tab lands on /console with "+"
	// highlighted as the way back in.
	async function closeTab(e: MouseEvent, tab: WorkspaceTab) {
		e.preventDefault();
		e.stopPropagation();
		const before = tabsStore.tabs.map((t) => t.slug);
		const idx = before.indexOf(tab.slug);
		const wasActive = tab.slug === currentSlug;
		try {
			await tabsStore.close(tab.slug);
		} catch {
			return;
		}
		if (!wasActive) return;
		const after = tabsStore.tabs;
		if (after.length === 0) {
			// Set once the navigation has landed: /console has no TopBar, so
			// the console page shows it on its own create button and clears it
			// when it unmounts.
			await goto('/console');
			uiStore.highlightAddWorkspace();
			return;
		}
		const neighbour = idx > 0 ? before[idx - 1] : before[idx + 1];
		const landing = after.find((t) => t.slug === neighbour) ?? after[0];
		goto(workspaceRestoreTarget(landing));
	}


	// ── Color palette ────────────────────────────────────────────────────
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

	// ── Cleanup ──────────────────────────────────────────────────────────
	$effect(() => {
		return () => {
			if (dropClickGuardTimer) {
				clearTimeout(dropClickGuardTimer);
				dropClickGuardTimer = null;
			}
		};
	});

	// ── Theme ────────────────────────────────────────────────────────────
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

	// ── Workspace-link click ─────────────────────────────────────────────
	// Plain left-click is intercepted to restore the last-visited route
	// (TASK-754). Modifier-clicks fall through to the <a href> so users
	// can still cmd-click / middle-click into a fresh dashboard tab.
	// Click on the *current* workspace overrides the restore — gives the
	// user a way back to the workspace dashboard from any deep route.
	function handleWsClick(e: MouseEvent, ws: { slug: string; owner_username: string }) {
		// Suppress the synthetic click that fires after a drag finalizes
		// (see armDropClickGuard).
		if (dropClickGuard) {
			e.preventDefault();
			return;
		}
		if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
		e.preventDefault();
		// The second click of a double-click keeps the tab (handleTabDblClick)
		// rather than navigating a second time, which on the now-current
		// workspace would send you to its dashboard.
		if (e.detail > 1) return;
		const target =
			ws.slug === currentSlug
				? `/${ws.owner_username}/${ws.slug}`
				: workspaceRestoreTarget(ws);
		goto(target);
	}
</script>


<!--
	User menu — shared by the desktop and mobile branches (the two copies
	were previously duplicated verbatim). Migrated onto the shared Menu
	primitive (PLAN-2290 Phase 2 / TASK-2292): the primitive owns the
	panel skin, pointerdown outside-click, ESC via the escape stack,
	roving keyboard nav over [role^=menuitem], and focus-return to the
	trigger. Rows that navigate stay real <a>s (role="menuitem" so the
	roving nav picks them up); action rows are MenuItem buttons.

	The inner .user-dropdown wrapper is a styling hook, not a panel:
	UserMenuResources' scoped styles target :global(.user-dropdown)
	(see its <style> block), so the class must survive even though the
	Menu primitive now owns the panel itself.
-->
{#snippet userMenu()}
	{#if authStore.user}
		<div class="user-menu-container">
			<button
				class="user-trigger"
				bind:this={userTriggerEl}
				aria-haspopup="menu"
				aria-expanded={userMenuOpen}
				aria-label="User menu"
				onclick={() => (userMenuOpen = !userMenuOpen)}
			>
				<span class="user-avatar" style="background: {wsColor(authStore.user.name || authStore.user.email)}">
					{(authStore.user.name || authStore.user.email).charAt(0).toUpperCase()}
				</span>
			</button>

			<Menu
				open={userMenuOpen}
				onclose={closeUserMenu}
				trigger={userTriggerEl}
				mode="anchored"
				ariaLabel="User menu"
				suppressOutside={() => isDragging}
				bodyScroll
			>
				<!--
					SIGN OUT IS PINNED; everything above it scrolls (BUG-2985).

					The panel caps at 340px and used to be the scroller, so this
					menu — 393px of content — ended at the cap with a clean edge,
					no scrollbar (overlay scrollbars show nothing until you
					scroll) and no cue. "Connect a project…" and "Sign out" were
					below the cut: the menu looked complete and the reporter
					concluded there was no sign-out at all. Measured before the
					fix at client height 338 with Sign out rendering at y=409.

					Making the menu shorter would only move the cliff — Billing
					(cloud), Admin and the Resources block all come and go — so
					the account action is kept OUT of the scrolling region
					instead. The middle grows and scrolls; the divider and Sign
					out are always on screen.
				-->
				<div class="user-dropdown">
					<div class="user-dropdown-body">
					<div class="user-info">
						<span class="user-dropdown-name">{authStore.user?.name}</span>
						<span class="user-dropdown-email">{authStore.user?.email}</span>
					</div>
					<div class="dropdown-divider"></div>
					<a href="/console" class="menu-link" role="menuitem" onclick={closeUserMenu}>
						Workspaces
					</a>
					<a href="/console/settings" class="menu-link" role="menuitem" onclick={closeUserMenu}>
						Settings
					</a>
					{#if authStore.cloudMode}
						<a href="/console/billing" class="menu-link" role="menuitem" onclick={closeUserMenu}>
							Billing
						</a>
					{/if}
					{#if authStore.user?.role === 'admin'}
						<a href="/console/admin" class="menu-link" role="menuitem" onclick={closeUserMenu}>
							Admin
						</a>
					{/if}
					<!--
						Theme toggle deliberately does NOT close the menu — matches
						the pre-migration behavior (the label flips in place so the
						user can toggle back without reopening).
					-->
					<MenuItem onclick={toggleTheme}>
						{currentTheme === 'dark' ? 'Light mode' : 'Dark mode'}
					</MenuItem>

					<!--
						Resources block (TASK-905). Replaces the prior inline Cloud
						Support/Status pair — that block became a special case of
						the unified Resources component, which adds Docs, Changelog,
						and GitHub on Cloud and a trimmed Docs/GitHub list on
						self-hosted. Closes the product → marketing handoff seam.
					-->
					<UserMenuResources cloudMode={authStore.cloudMode} onclose={closeUserMenu} />

					<!--
						"Connect a project…" sits after the Resources block and just
						above the Sign-out divider — it's a CLI-onboarding action,
						semantically closer to Settings/Resources than to account
						actions, but visually we want it adjacent to the divider so
						it reads as a discrete action rather than another link.
					-->
					{#if workspaceStore.current?.slug}
						<MenuItem
							onclick={() => {
								closeUserMenu();
								connectOpen = true;
							}}
						>
							Connect a project…
						</MenuItem>
					{/if}
					</div>
					<div class="dropdown-divider"></div>
					<MenuItem danger onclick={handleLogout}>Sign out</MenuItem>
				</div>
			</Menu>
		</div>
	{/if}
{/snippet}

{#if !mobile}
	<!-- ── Desktop ────────────────────────────────────────────────────────── -->
	<header class="topbar">
		<div class="topbar-left">
			<PadLogo />
		</div>
		<!--
			The workspace tab bar (PLAN-3002 U3): the open set, in bar order,
			in ONE dndzone with plain horizontal scroll, then "+". The tab
			hrefs stay pointed at the workspace dashboard so a middle-click /
			cmd-click / "Open in new tab" lands on a clean dashboard; a plain
			left-click is intercepted to restore the last-visited route
			(TASK-754).
		-->
		<div class="workspace-row">
			<!-- svelte-ignore a11y_no_static_element_interactions -->
			<div
				class="workspace-list"
				use:dndzone={{
					items: dndTabs,
					flipDurationMs,
					type: 'topbar-workspace',
					dragDisabled: uiStore.isTouch
				}}
				onconsider={handleTabsConsider}
				onfinalize={handleTabsFinalize}
			>
				{#each dndTabs as tab (tab.id)}
					<div
						class="workspace-tab"
						class:active={tab.slug === currentSlug}
						class:ephemeral={tab.ephemeral}
						class:guest={tab.is_guest}
						data-ws-slug={tab.slug}
					>
						<a
							href="/{tab.owner_username}/{tab.slug}"
							class="workspace-item"
							class:active={tab.slug === currentSlug}
							title={tab.is_guest ? `${tab.name} (shared with you)` : tab.name}
							aria-current={tab.slug === currentSlug ? 'page' : undefined}
							onclick={(e) => handleWsClick(e, tab)}
							ondblclick={() => handleTabDblClick(tab)}
						>
							<span
								class="workspace-icon"
								style="background: {tab.slug === currentSlug
									? wsColor(tab.name)
									: 'transparent'}; color: {tab.slug === currentSlug
									? '#fff'
									: 'var(--text-secondary)'}; border-color: {wsColor(tab.name)}"
							>
								{wsInitial(tab.name)}
							</span>
							<span class="workspace-name">{tab.name}</span>
						</a>
						<button
							type="button"
							class="workspace-tab-close"
							aria-label="Close {tab.name}"
							title="Close"
							onclick={(e) => closeTab(e, tab)}
						>
							<span aria-hidden="true">×</span>
						</button>
					</div>
				{/each}
			</div>

			<button
				class="workspace-add"
				onclick={() => uiStore.openCreateWorkspace()}
				title="New workspace"
			>
				<span class="add-icon">+</span>
			</button>
		</div>

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
			{@render userMenu()}
		</div>

		<!--
			Modal lives OUTSIDE the dropdown so it doesn't unmount when the
			dropdown closes (closeUserMenu fires synchronously with opening
			the modal). Gated on workspaceStore.current?.slug since the
			modal needs a workspace to interpolate into the connect snippet.
		-->
		{#if workspaceStore.current?.slug}
			<ConnectWorkspaceModal
				bind:open={connectOpen}
				serverUrl={typeof window !== 'undefined' ? window.location.origin : ''}
				workspaceSlug={workspaceStore.current.slug}
				workspaceName={workspaceStore.current.name}
				mcpPublicUrl={authStore.mcpPublicUrl}
			/>
		{/if}
	</header>
{:else}
	<!-- ── Mobile ─────────────────────────────────────────────────────────── -->
	<!--
		Mobile TopBar is cramped, and horizontal-scrolling the full workspace
		list hides workspaces off-screen. Swap the list + add + reorder
		buttons for a single WorkspaceSwitcher trigger that opens a full-
		width BottomSheet (see TASK-637). Reorder is available on desktop.

		As of IDEA-1121 / TASK-1122 the mobile TopBar is the SOLE mobile
		header — it renders regardless of sidebar state (see +layout.svelte).
		The previous slim in-content `.mobile-header` (hamburger + switcher)
		was deleted; the hamburger lives here in the .topbar-left slot,
		replacing the PadLogo on mobile so the chrome stays a single coherent
		bar across both sidebar-open and sidebar-closed states.
	-->
	<header class="topbar topbar-mobile">
		<div class="topbar-left">
			<button
				class="mobile-hamburger"
				onclick={() => uiStore.toggleSidebar()}
				aria-label={uiStore.sidebarOpen ? 'Close sidebar' : 'Open sidebar'}
				aria-expanded={uiStore.sidebarOpen}
				title={uiStore.sidebarOpen ? 'Close sidebar' : 'Open sidebar'}
			>
				<svg width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
					<rect y="3" width="20" height="2" rx="1" fill="currentColor"/>
					<rect y="9" width="20" height="2" rx="1" fill="currentColor"/>
					<rect y="15" width="20" height="2" rx="1" fill="currentColor"/>
				</svg>
			</button>
		</div>
		<div class="mobile-switcher-slot">
			<!--
				This slot only renders inside the mobile TopBar, so force the
				mobile (BottomSheet) branch unconditionally rather than
				re-deriving it from the viewport.
			-->
			<WorkspaceSwitcher mobile={true} />
		</div>
		<!--
			Mobile-only search trigger (IDEA-1121 / TASK-1122). Desktop has
			the sidebar's search button + ⌘K hotkey; mobile had no search
			affordance until this button. Opens the global CommandPalette
			mounted in +layout.svelte via uiStore.openSearch().

			Also calls uiStore.onNavigate() — which closes the sidebar on
			mobile (ui.svelte.ts:87) — so picking a search result doesn't
			leave the sidebar overlay covering the destination page. Mirrors
			the desktop sidebar's search button (Sidebar.svelte:522). Caught
			by Codex review of the IDEA-1121 work.
		-->
		<button
			class="mobile-search-btn"
			onclick={() => { uiStore.openSearch(); uiStore.onNavigate(); }}
			aria-label="Search"
			title="Search"
		>
			<svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
				<circle cx="7" cy="7" r="4.5" stroke="currentColor" stroke-width="1.5"/>
				<path d="M10.5 10.5L13.5 13.5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/>
			</svg>
		</button>
		{@render userMenu()}

		<!-- Same Connect modal pattern as the desktop branch — see notes above. -->
		{#if workspaceStore.current?.slug}
			<ConnectWorkspaceModal
				bind:open={connectOpen}
				serverUrl={typeof window !== 'undefined' ? window.location.origin : ''}
				workspaceSlug={workspaceStore.current.slug}
				workspaceName={workspaceStore.current.name}
				mcpPublicUrl={authStore.mcpPublicUrl}
			/>
		{/if}
	</header>
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
		padding-right: var(--space-3);
	}

	/*
		The workspace tab bar (PLAN-3002 U3): the tab list and "+" as one
		centered group. `flex: 1 1 auto; min-width: 0` lets the row shrink
		below its content so the list scrolls instead of pushing the user
		menu off the bar.
	*/
	.workspace-row {
		display: flex;
		align-items: center;
		justify-content: center;
		flex: 1 1 auto;
		gap: 2px;
		min-width: 0;
	}

	/* One zone, plain horizontal scroll. The scrollbar is hidden: the bar is
	   a single row of chrome and scrolls by wheel, trackpad and drag. */
	.workspace-list {
		display: flex;
		align-items: center;
		gap: 2px;
		min-width: 0;
		max-width: 100%;
		overflow-x: auto;
		scrollbar-width: none;
	}
	.workspace-list::-webkit-scrollbar {
		display: none;
	}

	/* A tab: the workspace link plus its close button, which shows on hover,
	   on keyboard focus within the tab, and always on the active tab. */
	.workspace-tab {
		position: relative;
		display: flex;
		align-items: center;
		flex-shrink: 0;
		border-radius: var(--radius);
	}
	.workspace-tab-close {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 18px;
		height: 18px;
		margin-left: -4px;
		margin-right: 2px;
		padding: 0;
		border: none;
		border-radius: 50%;
		background: transparent;
		color: var(--text-muted);
		font-size: 0.9em;
		line-height: 1;
		cursor: pointer;
		opacity: 0;
		transition: opacity 0.15s, background 0.15s, color 0.15s;
	}
	.workspace-tab:hover .workspace-tab-close,
	.workspace-tab:focus-within .workspace-tab-close,
	.workspace-tab.active .workspace-tab-close {
		opacity: 1;
	}
	.workspace-tab-close:hover,
	.workspace-tab-close:focus-visible {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	/* An ephemeral tab (opened by a landing, not kept yet) reads italic
	   (PLAN-3002 Q9). */
	.workspace-tab.ephemeral .workspace-name {
		font-style: italic;
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
	/* Truncate long workspace names so one tab cannot claim the bar. */
	.workspace-list .workspace-name {
		max-width: 200px;
		overflow: hidden;
		text-overflow: ellipsis;
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

	/* Mobile TopBar — slot that hosts the <WorkspaceSwitcher /> chip.
	   Flex-grows so the switcher trigger stretches to fill the gap between
	   the absolute-positioned logo and the user avatar. */
	.mobile-switcher-slot {
		flex: 1;
		min-width: 0;
		display: flex;
		align-items: center;
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

	/*
		Mobile-only search button (IDEA-1121 / TASK-1122). Sized and
		styled to match .collapse-btn so the mobile right cluster reads
		as a coherent set of icon controls. Only ever rendered inside
		the .topbar-mobile <header>, so no media query needed here.
	*/
	.mobile-search-btn {
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
		transition: color 0.15s, background 0.15s;
	}
	.mobile-search-btn:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	/*
		Mobile-only hamburger (IDEA-1121 / TASK-1122). Replaces the desktop
		PadLogo in the .topbar-left slot on mobile because the hamburger is
		the universal mobile-app primary affordance and space is tight. Calls
		uiStore.toggleSidebar() — works in both directions (open when
		closed, close when open) so a user who opens the sidebar can dismiss
		it from the same button without hunting for an X. Slightly larger
		than the icon buttons on the right (32×32 vs 28×28) because it's the
		primary nav target — easier to thumb-tap and matches typical mobile
		hamburger sizing. Color tokens mirror the right-cluster icon buttons.
	*/
	.mobile-hamburger {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 32px;
		height: 32px;
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		cursor: pointer;
		padding: 0;
		background: none;
		border: none;
		transition: color 0.15s, background 0.15s;
	}
	.mobile-hamburger:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	/* Anchored-mode wrapper for the user Menu — the primitive's panel is
	   position: absolute against this. */
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

	/* Panel skin/positioning now lives in the Menu primitive. The
	   .user-dropdown class survives markup-side as a styling hook for
	   UserMenuResources' :global(.user-dropdown) rules. */

	/* BUG-2985: the panel no longer scrolls (Menu's `bodyScroll`), so this
	   column does — the body takes the space that is left and Sign out keeps
	   its own, whatever the body holds. `min-height: 0` is load-bearing: a
	   flex item's default `min-height: auto` refuses to shrink below its
	   content, so without it the body would push the pinned row back out of
	   the panel and restore the bug with extra steps. */
	.user-dropdown {
		display: flex;
		flex-direction: column;
		min-height: 0;
		max-height: 100%;
	}

	.user-dropdown-body {
		flex: 1 1 auto;
		min-height: 0;
		overflow-y: auto;
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

	/* Anchor rows inside the user Menu. Links stay real <a>s (they
	   navigate; role="menuitem" keeps them in the primitive's roving
	   keyboard nav), so they can't be MenuItem buttons — mirror
	   MenuItem's .mi metrics here so link rows and button rows read
	   as one set. */
	.menu-link {
		display: flex;
		align-items: center;
		width: 100%;
		padding: 7px 9px;
		border-radius: var(--radius-sm);
		font-size: 13px;
		color: var(--text-primary);
		text-decoration: none;
	}
	.menu-link:hover,
	.menu-link:focus-visible {
		background: var(--bg-hover);
		text-decoration: none;
		outline: none;
	}
	@media (pointer: coarse) {
		.menu-link {
			padding: 11px 10px;
		}
	}

</style>
