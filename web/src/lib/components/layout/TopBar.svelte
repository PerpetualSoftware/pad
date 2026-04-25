<script lang="ts">
	import { dndzone, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { api } from '$lib/api/client';
	import type { Workspace } from '$lib/types';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import PadLogo from '$lib/components/layout/PadLogo.svelte';
	import WorkspaceSwitcher from '$lib/components/layout/WorkspaceSwitcher.svelte';
	import { workspaceRestoreTarget } from '$lib/utils/workspace-route';

	let { mobile = false }: { mobile?: boolean } = $props();

	let userMenuOpen = $state(false);
	let currentTheme = $state<'dark' | 'light'>('dark');

	let currentSlug = $derived(workspaceStore.current?.slug ?? '');

	// ── DnD + overflow state ─────────────────────────────────────────────
	// Full ordered list, mirrors the store when idle and is mutated by
	// dnd zones during reorder. Source of truth for persistence.
	let dndWorkspaces: Workspace[] = $state([]);
	let isDragging = $state(false);
	// Drop cooldown blocks store→local sync for a moment after a drop so
	// SSE refresh of the workspace list doesn't fight the just-written
	// order. Mirrors BoardView's pattern.
	let dropCooldown = $state(false);
	const flipDurationMs = 150;

	// Sync from store when not actively reordering and not in cooldown.
	$effect(() => {
		if (!isDragging && !dropCooldown) {
			dndWorkspaces = [...workspaceStore.workspaces];
		}
	});

	// ── Width measurement ────────────────────────────────────────────────
	// We replaced the previous `overflow-x: auto` scroll with a "priority+"
	// pattern: pills that don't fit move into a `…` overflow menu. To
	// decide which pills fit, we measure each pill's natural width once in
	// a hidden ghost row, then walk the ordered list against the visible
	// container's width. ResizeObserver re-derives on container width
	// change (window resize, sidebar collapse, etc).
	let containerEl: HTMLDivElement | null = $state(null);
	let ghostEl: HTMLDivElement | null = $state(null);
	let availableWidth = $state(0);
	let pillWidths = $state(new Map<string, number>());

	// Reserve width for the `…` trigger so the visible/overflow split is
	// stable — see the rendering note on `.overflow-trigger-wrap.hidden`.
	const TRIGGER_RESERVATION = 32;
	const GAP = 2; // matches `.workspace-list` CSS gap

	$effect(() => {
		if (!containerEl) return;
		const ro = new ResizeObserver((entries) => {
			for (const entry of entries) {
				availableWidth = entry.contentRect.width;
			}
		});
		ro.observe(containerEl);
		return () => ro.disconnect();
	});

	// Re-measure pill widths whenever the ghost row's children change
	// (workspaces added/removed/renamed). Cached per slug.
	$effect(() => {
		// Re-run when dndWorkspaces changes — depend on the array reference
		// and length so renames/reorders trigger remeasurement.
		void dndWorkspaces.length;
		void dndWorkspaces.map((w) => w.slug + '|' + w.name).join(',');
		if (!ghostEl) return;
		// Defer until after paint so the ghost row has laid out.
		const raf = requestAnimationFrame(() => {
			const next = new Map<string, number>();
			for (const child of Array.from(ghostEl!.querySelectorAll<HTMLElement>('[data-ws-slug]'))) {
				const slug = child.dataset.wsSlug;
				if (slug) next.set(slug, child.offsetWidth);
			}
			pillWidths = next;
		});
		return () => cancelAnimationFrame(raf);
	});

	// ── Visible / overflow split ────────────────────────────────────────
	// Always reserves trigger width when there's overflow, AND renders the
	// trigger as `visibility: hidden` even when empty so layout is stable.
	// This avoids oscillation between "trigger hidden → list grows → all
	// fit → trigger hidden" and the next frame's "all fit → trigger shown
	// (?) → ...". Stable layout > shaving 28px when not needed.
	let propVisibleWorkspaces = $derived.by(() => {
		if (!availableWidth || pillWidths.size === 0) return dndWorkspaces;

		// Total width if we tried to fit everything (no trigger).
		let total = 0;
		for (let i = 0; i < dndWorkspaces.length; i++) {
			total += pillWidths.get(dndWorkspaces[i].slug) ?? 0;
			if (i > 0) total += GAP;
		}
		if (total <= availableWidth) return dndWorkspaces;

		// Some pills overflow — reserve trigger room.
		const budget = availableWidth - TRIGGER_RESERVATION - GAP;
		const activeSlug = workspaceStore.current?.slug ?? '';

		// Pin active: claim its width up front regardless of position.
		let consumed = 0;
		const activeWidth =
			activeSlug && pillWidths.has(activeSlug) ? pillWidths.get(activeSlug)! : 0;
		const haveActive = !!activeSlug && dndWorkspaces.some((w) => w.slug === activeSlug);
		if (haveActive) {
			consumed = activeWidth;
		}

		const visible: Workspace[] = [];
		for (const ws of dndWorkspaces) {
			if (ws.slug === activeSlug) {
				visible.push(ws);
				continue;
			}
			const w = pillWidths.get(ws.slug) ?? 0;
			const addGap = visible.length > 0 || haveActive ? GAP : 0;
			if (consumed + addGap + w <= budget) {
				consumed += addGap + w;
				visible.push(ws);
			}
			// Else: overflows — falls into the overflow set below.
		}
		return visible;
	});

	let propOverflowWorkspaces = $derived.by(() => {
		const visibleSlugs = new Set(propVisibleWorkspaces.map((w) => w.slug));
		return dndWorkspaces.filter((w) => !visibleSlugs.has(w.slug));
	});

	// DnD-mutable slices that mirror the derived split when idle. svelte-
	// dnd-action wants its own array per zone; consider/finalize update
	// these slices, and persistGlobalOrder folds them back into a single
	// linear order to push to the API.
	let visibleZone: Workspace[] = $state([]);
	let overflowZone: Workspace[] = $state([]);
	let triggerZone: Workspace[] = $state([]); // single-slot drop target for "drop on …"

	$effect(() => {
		const v = propVisibleWorkspaces;
		const o = propOverflowWorkspaces;
		if (!isDragging && !dropCooldown) {
			visibleZone = [...v];
			overflowZone = [...o];
			triggerZone = [];
		}
	});

	// ── Overflow menu state ──────────────────────────────────────────────
	let overflowOpen = $state(false);
	let overflowTriggerEl: HTMLButtonElement | null = $state(null);
	let springLoadTimer: ReturnType<typeof setTimeout> | null = null;
	const SPRING_LOAD_MS = 400;

	function clearSpringLoad() {
		if (springLoadTimer) {
			clearTimeout(springLoadTimer);
			springLoadTimer = null;
		}
	}

	function closeOverflow() {
		overflowOpen = false;
		clearSpringLoad();
	}

	// ── DnD handlers ─────────────────────────────────────────────────────
	function handleVisibleConsider(e: CustomEvent<DndEvent<Workspace>>) {
		visibleZone = e.detail.items;
		isDragging = true;
	}

	async function handleVisibleFinalize(e: CustomEvent<DndEvent<Workspace>>) {
		visibleZone = e.detail.items;
		isDragging = false;
		clearSpringLoad();
		schedulePersist();
	}

	function handleOverflowConsider(e: CustomEvent<DndEvent<Workspace>>) {
		overflowZone = e.detail.items;
		isDragging = true;
	}

	async function handleOverflowFinalize(e: CustomEvent<DndEvent<Workspace>>) {
		let next = e.detail.items;
		// Active-pin rule: if active was dragged into overflow, snap it
		// back to visible and don't persist that move. The active pill is
		// the only "you are here" cue in the bar — it must always render.
		const activeSlug = workspaceStore.current?.slug;
		if (activeSlug && next.some((w) => w.slug === activeSlug)) {
			const active = next.find((w) => w.slug === activeSlug)!;
			next = next.filter((w) => w.slug !== activeSlug);
			if (!visibleZone.some((w) => w.slug === activeSlug)) {
				visibleZone = [...visibleZone, active];
			}
		}
		overflowZone = next;
		isDragging = false;
		clearSpringLoad();
		schedulePersist();
	}

	// Trigger zone — single-slot drop target. While a drag is hovering,
	// schedule the spring-load timer to auto-open the menu so the user
	// can place the dropped item at a precise position. If the user drops
	// on the trigger before the timer fires, the dropped item is appended
	// to the end of overflow.
	function handleTriggerConsider(e: CustomEvent<DndEvent<Workspace>>) {
		triggerZone = e.detail.items;
		isDragging = true;

		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		const hovering = triggerZone.some((item: any) => !!item[SHADOW_ITEM_MARKER_PROPERTY_NAME]);
		if (hovering && !overflowOpen && !springLoadTimer) {
			springLoadTimer = setTimeout(() => {
				springLoadTimer = null;
				overflowOpen = true;
			}, SPRING_LOAD_MS);
		} else if (!hovering) {
			clearSpringLoad();
		}
	}

	async function handleTriggerFinalize(e: CustomEvent<DndEvent<Workspace>>) {
		const dropped = e.detail.items.filter(
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			(item: any) => !item[SHADOW_ITEM_MARKER_PROPERTY_NAME]
		);
		triggerZone = [];
		clearSpringLoad();

		if (dropped.length > 0) {
			// Active-pin under "drop on trigger": same rule as overflow zone.
			const activeSlug = workspaceStore.current?.slug;
			const droppedSafe = activeSlug
				? dropped.filter((w) => w.slug !== activeSlug)
				: dropped;
			if (droppedSafe.length > 0) {
				overflowZone = [...overflowZone, ...droppedSafe];
			}
		}
		isDragging = false;
		schedulePersist();
	}

	// Both zones fire `finalize` for a cross-zone drag. Coalesce both into
	// a single persist call to avoid racing API writes for one user action.
	let persistScheduled = false;
	function schedulePersist() {
		if (persistScheduled) return;
		persistScheduled = true;
		queueMicrotask(async () => {
			persistScheduled = false;
			await persistGlobalOrder();
		});
	}

	async function persistGlobalOrder() {
		// svelte-dnd-action tags shadow items with SHADOW_ITEM_MARKER_PROPERTY_NAME
		// during drag — strip them before we persist or update local state.
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		const stripShadow = (arr: Workspace[]): Workspace[] =>
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			arr.filter((x) => !(x as any)[SHADOW_ITEM_MARKER_PROPERTY_NAME]);
		const visible = stripShadow(visibleZone);
		const overflow = stripShadow(overflowZone);
		// Dedupe in case a cross-zone drag transiently appears in both
		// (svelte-dnd-action shadow handling should prevent this, but
		// belt-and-braces).
		const seen = new Set<string>();
		const fullOrder: Workspace[] = [];
		for (const ws of [...visible, ...overflow]) {
			if (!seen.has(ws.slug)) {
				seen.add(ws.slug);
				fullOrder.push(ws);
			}
		}
		dndWorkspaces = fullOrder;
		dropCooldown = true;

		const updates = fullOrder.map((ws, i) => ({ slug: ws.slug, sort_order: i }));
		try {
			await api.workspaces.reorder(updates);
			await workspaceStore.loadAll();
		} catch {
			// Restore from store on failure.
			dndWorkspaces = [...workspaceStore.workspaces];
		}
		setTimeout(() => {
			dropCooldown = false;
		}, 1000);
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
	function handleWsClick(e: MouseEvent, ws: Workspace) {
		if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
		e.preventDefault();
		closeOverflow();
		goto(workspaceRestoreTarget(ws));
	}

	// ── Overflow menu keyboard ───────────────────────────────────────────
	function handleOverflowKeydown(e: KeyboardEvent) {
		if (!overflowOpen) return;
		if (e.key === 'Escape') {
			e.preventDefault();
			closeOverflow();
			overflowTriggerEl?.focus();
			return;
		}
		if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
			e.preventDefault();
			const items = Array.from(
				document.querySelectorAll<HTMLElement>('#workspace-overflow-menu [role="menuitem"]')
			);
			if (items.length === 0) return;
			const active = document.activeElement as HTMLElement | null;
			const idx = active ? items.indexOf(active) : -1;
			const next =
				e.key === 'ArrowDown'
					? items[(idx + 1 + items.length) % items.length]
					: items[(idx - 1 + items.length) % items.length];
			next.focus();
		}
	}
</script>

<svelte:window
	onclick={(e) => {
		const target = e.target as HTMLElement;
		if (userMenuOpen && !target.closest('.user-menu-container')) {
			closeUserMenu();
		}
		if (
			overflowOpen &&
			!target.closest('#workspace-overflow-menu') &&
			!target.closest('.overflow-trigger-wrap')
		) {
			closeOverflow();
		}
	}}
	onkeydown={handleOverflowKeydown}
/>

{#if !mobile}
	<!-- ── Desktop ────────────────────────────────────────────────────────── -->
	<header class="topbar">
		<div class="topbar-left">
			<PadLogo />
		</div>
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div
			class="workspace-list"
			bind:this={containerEl}
			use:dndzone={{
				items: visibleZone,
				flipDurationMs,
				type: 'topbar-workspace',
				dragDisabled: uiStore.isTouch
			}}
			onconsider={handleVisibleConsider}
			onfinalize={handleVisibleFinalize}
		>
			{#each visibleZone as ws (ws.id)}
				<!--
					href stays pointed at the workspace dashboard so a
					middle-click / cmd-click / right-click → "Open in new
					tab" lands on a clean dashboard. Plain left-click is
					intercepted to restore the last-visited route in that
					workspace (TASK-754) — the previous behavior was a
					hard `<a>` nav to the dashboard, which silently
					overwrote the saved deep route in localStorage on
					every workspace switch.
				-->
				<a
					href="/{ws.owner_username}/{ws.slug}"
					class="workspace-item"
					class:active={ws.slug === currentSlug}
					title={ws.name}
					onclick={(e) => handleWsClick(e, ws)}
				>
					<span
						class="workspace-icon"
						style="background: {ws.slug === currentSlug
							? wsColor(ws.name)
							: 'transparent'}; color: {ws.slug === currentSlug
							? '#fff'
							: 'var(--text-secondary)'}; border-color: {wsColor(ws.name)}"
					>
						{wsInitial(ws.name)}
					</span>
					<span class="workspace-name">{ws.name}</span>
				</a>
			{/each}
		</div>

		<!--
			Overflow trigger. Always rendered (so layout stays stable as
			workspaces are added/removed) but visually hidden when nothing
			overflows. The wrapper is a 3rd dndzone with the same `type`
			as the visible row and the menu, so dropping on the trigger
			appends to overflow. Spring-loaded auto-open on hover-during-
			drag is implemented in handleTriggerConsider.
		-->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div
			class="overflow-trigger-wrap"
			class:hidden={overflowZone.length === 0}
			use:dndzone={{
				items: triggerZone,
				flipDurationMs,
				type: 'topbar-workspace',
				dragDisabled: uiStore.isTouch || overflowZone.length === 0
			}}
			onconsider={handleTriggerConsider}
			onfinalize={handleTriggerFinalize}
		>
			<button
				class="overflow-trigger"
				bind:this={overflowTriggerEl}
				aria-haspopup="menu"
				aria-expanded={overflowOpen}
				aria-controls="workspace-overflow-menu"
				aria-label="Show {overflowZone.length} more workspace{overflowZone.length === 1
					? ''
					: 's'}"
				title="More workspaces"
				onclick={() => (overflowOpen = !overflowOpen)}
			>
				<span class="overflow-dots" aria-hidden="true">⋯</span>
			</button>
		</div>

		{#if overflowOpen && overflowZone.length > 0}
			<!-- svelte-ignore a11y_no_static_element_interactions -->
			<div
				id="workspace-overflow-menu"
				class="overflow-menu"
				role="menu"
				use:dndzone={{
					items: overflowZone,
					flipDurationMs,
					type: 'topbar-workspace',
					dragDisabled: uiStore.isTouch
				}}
				onconsider={handleOverflowConsider}
				onfinalize={handleOverflowFinalize}
			>
				{#each overflowZone as ws (ws.id)}
					<a
						href="/{ws.owner_username}/{ws.slug}"
						class="overflow-menu-item"
						class:active={ws.slug === currentSlug}
						role="menuitem"
						title={ws.name}
						onclick={(e) => handleWsClick(e, ws)}
					>
						<span
							class="workspace-icon"
							style="background: {ws.slug === currentSlug
								? wsColor(ws.name)
								: 'transparent'}; color: {ws.slug === currentSlug
								? '#fff'
								: 'var(--text-secondary)'}; border-color: {wsColor(ws.name)}"
						>
							{wsInitial(ws.name)}
						</span>
						<span class="workspace-name">{ws.name}</span>
					</a>
				{/each}
			</div>
		{/if}

		<button
			class="workspace-add"
			onclick={() => uiStore.openCreateWorkspace()}
			title="New workspace"
		>
			<span class="add-icon">+</span>
		</button>

		<!--
			Hidden ghost row used to measure each pill's natural width.
			Rendered with `position: absolute; visibility: hidden` so it
			takes its own layout offscreen and doesn't influence the real
			.workspace-list. Items are keyed by id for stable identity.
		-->
		<div class="workspace-ghost" bind:this={ghostEl} aria-hidden="true">
			{#each dndWorkspaces as ws (ws.id)}
				<span class="workspace-item ghost-item" data-ws-slug={ws.slug}>
					<span
						class="workspace-icon"
						style="border-color: {wsColor(ws.name)}"
					>
						{wsInitial(ws.name)}
					</span>
					<span class="workspace-name">{ws.name}</span>
				</span>
			{/each}
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
							{#if authStore.cloudMode}
								<div class="dropdown-divider"></div>
								<a
									href="mailto:support@getpad.dev"
									class="dropdown-item"
									onclick={closeUserMenu}
								>
									Support
								</a>
								<a
									href="https://status.getpad.dev"
									target="_blank"
									rel="noopener noreferrer"
									class="dropdown-item"
									onclick={closeUserMenu}
								>
									Status
								</a>
							{/if}
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
	<!--
		Mobile TopBar is cramped, and horizontal-scrolling the full workspace
		list hides workspaces off-screen. Swap the list + add + reorder
		buttons for a single WorkspaceSwitcher trigger that opens a full-
		width BottomSheet (see TASK-637). Reorder is available on desktop.
	-->
	<header class="topbar topbar-mobile">
		<div class="topbar-left">
			<PadLogo />
		</div>
		<div class="mobile-switcher-slot">
			<!--
				Force the mobile branch. TopBar's mobile/desktop decision uses
				`uiStore.isMobile` (≤768px) but WorkspaceSwitcher's own query
				is ≤639.98px — without this prop, 640–768px viewports would
				render the desktop dropdown inside a mobile layout.
			-->
			<WorkspaceSwitcher mobile={true} />
		</div>
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
						{#if authStore.cloudMode}
							<div class="dropdown-divider"></div>
							<a
								href="mailto:support@getpad.dev"
								class="dropdown-item"
								onclick={closeUserMenu}
							>
								Support
							</a>
							<a
								href="https://status.getpad.dev"
								target="_blank"
								rel="noopener noreferrer"
								class="dropdown-item"
								onclick={closeUserMenu}
							>
								Status
							</a>
						{/if}
						<div class="dropdown-divider"></div>
						<button class="dropdown-item logout" onclick={handleLogout}>
							Sign out
						</button>
					</div>
				{/if}
			</div>
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
		Workspace list — pills that fit. No more `overflow-x: auto`; pills
		that don't fit are routed into the .overflow-menu via
		propVisibleWorkspaces / propOverflowWorkspaces. See script for the
		measurement + split logic.
	*/
	.workspace-list {
		display: flex;
		align-items: center;
		gap: 2px;
		min-width: 0;
		max-width: 100%;
		padding-right: var(--space-2);
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

	/*
		Overflow trigger — visually a sibling of .workspace-list and
		.workspace-add. Always rendered to keep layout stable; visually
		hidden via `visibility: hidden` when overflowZone is empty (also
		blocks pointer events so a hidden trigger isn't clickable).
	*/
	.overflow-trigger-wrap {
		display: flex;
		align-items: center;
		flex-shrink: 0;
	}
	.overflow-trigger-wrap.hidden {
		visibility: hidden;
		pointer-events: none;
	}
	.overflow-trigger {
		display: flex;
		align-items: center;
		justify-content: center;
		width: 24px;
		height: 24px;
		border-radius: 50%;
		flex-shrink: 0;
		color: var(--text-muted);
		border: 2px solid var(--border);
		background: transparent;
		cursor: pointer;
		transition: border-color 0.15s, color 0.15s, background 0.15s;
		padding: 0;
	}
	.overflow-trigger:hover,
	.overflow-trigger[aria-expanded='true'] {
		border-color: var(--text-secondary);
		color: var(--text-primary);
		background: var(--bg-hover);
	}
	.overflow-dots {
		font-size: 0.95em;
		font-weight: 700;
		line-height: 1;
		letter-spacing: 0;
	}

	/*
		Overflow menu — anchored under the trigger. Positioned absolutely
		from .topbar so it floats over surrounding chrome. Reuses the
		.user-dropdown look for visual consistency.
	*/
	.overflow-menu {
		position: absolute;
		top: calc(100% + 6px);
		min-width: 220px;
		max-height: 60vh;
		overflow-y: auto;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 8px 24px rgba(0, 0, 0, 0.3);
		z-index: 50;
		padding: var(--space-1);
		display: flex;
		flex-direction: column;
		gap: 1px;
		animation: dropdown-in 0.12s ease-out;
		/*
			Approximate horizontal anchor: place near the trigger which
			sits between the list and the + button. Trigger is centered in
			the topbar by flexbox; menu uses translateX to nudge under it.
			Precise positioning isn't critical — the menu just needs to be
			obviously associated with the trigger.
		*/
		left: 50%;
		transform: translateX(-50%);
	}
	.overflow-menu-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		text-decoration: none;
		color: var(--text-secondary);
		white-space: nowrap;
		transition: background 0.1s, color 0.1s;
	}
	.overflow-menu-item:hover,
	.overflow-menu-item:focus-visible {
		background: var(--bg-hover);
		color: var(--text-primary);
		text-decoration: none;
		outline: none;
	}
	.overflow-menu-item.active {
		background: var(--bg-active, var(--bg-hover));
		color: var(--text-primary);
	}
	.topbar:not(.topbar-mobile) .overflow-menu-item {
		cursor: grab;
	}
	.topbar:not(.topbar-mobile) .overflow-menu-item:active {
		cursor: grabbing;
	}

	/*
		Ghost measurement row. Off-screen; layout-active only enough to
		yield real offsetWidth values for each pill.
	*/
	.workspace-ghost {
		position: absolute;
		visibility: hidden;
		pointer-events: none;
		left: -9999px;
		top: 0;
		display: flex;
		align-items: center;
		gap: 2px;
		white-space: nowrap;
	}
	.ghost-item {
		flex-shrink: 0;
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

</style>
