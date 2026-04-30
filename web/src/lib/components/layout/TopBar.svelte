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
	import UserMenuResources from '$lib/components/layout/UserMenuResources.svelte';
	import ConnectWorkspaceModal from '$lib/components/ConnectWorkspaceModal.svelte';
	import { workspaceRestoreTarget } from '$lib/utils/workspace-route';

	let { mobile = false }: { mobile?: boolean } = $props();

	let userMenuOpen = $state(false);
	let currentTheme = $state<'dark' | 'light'>('dark');
	let connectOpen = $state(false);

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
	// a hidden ghost row, then walk the ordered list against the centered
	// row's width. ResizeObserver is on `.workspace-row` (the centered
	// flex:1 wrapper that holds pills + trigger + add) so we measure the
	// real available row, not just the content-collapsed pill list. Re-
	// derives on container width change (window resize, sidebar collapse).
	let rowEl: HTMLDivElement | null = $state(null);
	let ghostEl: HTMLDivElement | null = $state(null);
	let availableWidth = $state(0);
	let pillWidths = $state(new Map<string, number>());

	// Chrome that shares the centered row with the pills. Broken into
	// named parts (instead of a single magic 72) so a styling change in
	// one of the contributing CSS rules has an obvious place to update,
	// and the math stays auditable. Numbers reflect the current CSS box
	// model and `var(--space-2)` token (8px); update both together.
	const TRIGGER_WIDTH = 28; // .overflow-trigger: 24 inner + 2px border × 2
	const ADD_BUTTON_WIDTH = 28; // .workspace-add: 24 inner + 2px border × 2
	const ROW_GAP = 2; // .workspace-row gap (between list, anchor, add)
	const LIST_PADDING_RIGHT = 8; // .workspace-list padding-right (var(--space-2))
	const CHROME_RESERVATION =
		TRIGGER_WIDTH + ADD_BUTTON_WIDTH + 2 * ROW_GAP + LIST_PADDING_RIGHT; // = 68
	const GAP = 2; // matches `.workspace-row` and `.workspace-list` CSS gap

	$effect(() => {
		if (!rowEl) return;
		const ro = new ResizeObserver((entries) => {
			for (const entry of entries) {
				availableWidth = entry.contentRect.width;
			}
		});
		ro.observe(rowEl);
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
	// Pills compete for `budget = availableWidth - CHROME_RESERVATION`.
	// Chrome (trigger + add button + gaps) is always reserved, so layout
	// is stable across overflow / no-overflow transitions — the trigger
	// itself uses visibility: hidden when there's nothing to overflow.
	let propVisibleWorkspaces = $derived.by(() => {
		if (!availableWidth || pillWidths.size === 0) return dndWorkspaces;

		// Pill space = row width minus the chrome that sits beside the pills.
		const budget = availableWidth - CHROME_RESERVATION;

		// Total width if we tried to fit everything.
		let total = 0;
		for (let i = 0; i < dndWorkspaces.length; i++) {
			total += pillWidths.get(dndWorkspaces[i].slug) ?? 0;
			if (i > 0) total += GAP;
		}
		if (total <= budget) return dndWorkspaces;

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
	// The menu's `.open` class is bound to `overflowOpen || isDragging ||
	// dragArmed`:
	//   • `overflowOpen` — user clicked the … trigger.
	//   • `isDragging`   — drag in progress, auto-show during drag.
	//   • `dragArmed`    — mousedown on a draggable pill but drag hasn't
	//      started yet. Pre-arm matters because `svelte-dnd-action` does
	//      its drop-zone hit-testing with `document.elementsFromPoint`,
	//      which excludes elements with `pointer-events: none`. If the
	//      menu is still `pointer-events: none` at the moment the drag
	//      threshold is crossed, the library evaluates the zone as
	//      non-interactive and stops considering it as a hit target for
	//      the rest of this drag — the user can see the menu but drops
	//      and FLIP animations on existing items don't register. Flipping
	//      `.open` synchronously on mousedown sidesteps that race.
	// The menu DOM itself is mounted whenever `overflowZone.length > 0`,
	// so the dndzone is registered well before any drag starts.
	let overflowOpen = $state(false);
	let dragArmed = $state(false);
	let overflowTriggerEl: HTMLButtonElement | null = $state(null);
	// Single tracked timer for the post-drop cooldown — re-armed on each
	// drop so two drops within 1s don't see the first timeout end the
	// cooldown for the second (Codex round 1 MEDIUM).
	let cooldownTimer: ReturnType<typeof setTimeout> | null = null;

	// Drag-end click suppression. After a drop, the browser fires a
	// synthetic `click` on the original mousedown target (the dragged
	// `<a>` pill). svelte-dnd-action does NOT cancel that click, so
	// without this guard the click reaches `handleWsClick`, calls
	// `goto()`, and navigates to the dragged workspace's restore route
	// — which the user experiences as the current page refreshing
	// every time they reorder workspaces. `armDropClickGuard()` is
	// called at the end of every finalize handler; `handleWsClick`
	// bails out while the guard is active.
	let dropClickGuard = false;
	let dropClickGuardTimer: ReturnType<typeof setTimeout> | null = null;
	function armDropClickGuard() {
		dropClickGuard = true;
		if (dropClickGuardTimer) clearTimeout(dropClickGuardTimer);
		// 100ms is comfortably longer than the gap between mouseup and
		// the synthetic click, but short enough that a deliberate click
		// shortly after a drag isn't accidentally swallowed.
		dropClickGuardTimer = setTimeout(() => {
			dropClickGuard = false;
			dropClickGuardTimer = null;
		}, 100);
	}

	function closeOverflow() {
		overflowOpen = false;
	}

	function armMenuForDrag(e: MouseEvent) {
		// Primary button only — secondary clicks shouldn't pre-open.
		if (e.button !== 0) return;
		dragArmed = true;
	}

	function disarmMenu() {
		// Cleared on mouseup if no drag actually started. If a drag DID
		// start, `isDragging` is true here and the corresponding finalize
		// handler clears `dragArmed` itself when the drop completes.
		if (!isDragging) dragArmed = false;
	}

	// Roll back any cooldown state set by an earlier (source-zone) finalize
	// when a later (target-zone) finalize takes the active-pin reject path.
	// svelte-dnd-action doesn't guarantee finalize ordering across zones, so
	// the visible-zone finalize may run first, set `dropCooldown = true`,
	// and `schedulePersist`. If the overflow/trigger finalize then rejects,
	// `cancelPersist` skips the persist microtask — which means the
	// cooldown timer (only armed inside `persistGlobalOrder`) is never set,
	// and `dropCooldown` stays stuck `true`. Both store→local sync effects
	// are gated on `!dropCooldown`, so a stuck flag detaches the topbar
	// from `workspaceStore.workspaces` until the next successful drop.
	function clearCooldownAfterRejection() {
		dropCooldown = false;
		if (cooldownTimer) {
			clearTimeout(cooldownTimer);
			cooldownTimer = null;
		}
	}

	// Visual open state for the overflow menu — used both for the `.open`
	// CSS class and for `aria-expanded` on the trigger so screen readers
	// see the same state as sighted users. (The keyboard menu navigation
	// stays gated on `overflowOpen` only, since drag-driven opens aren't
	// keyboard-reachable in the first place.)
	let menuVisible = $derived(overflowOpen || isDragging || dragArmed);

	// ── DnD handlers ─────────────────────────────────────────────────────
	function handleVisibleConsider(e: CustomEvent<DndEvent<Workspace>>) {
		visibleZone = e.detail.items;
		isDragging = true;
		// Fresh drag — clear any stale rejection flag from a prior cycle.
		dragRejected = false;
	}

	async function handleVisibleFinalize(e: CustomEvent<DndEvent<Workspace>>) {
		// If the OTHER zone (overflow or trigger) already detected an
		// active-pin rejection and reset state, don't clobber visibleZone
		// with the post-drag items here — they exclude active. Just
		// finish the drag cleanly. (svelte-dnd-action does not guarantee
		// the order in which the source and target finalize events fire.)
		if (dragRejected) {
			dragRejected = false;
			dragArmed = false;
			isDragging = false;
			armDropClickGuard();
			return;
		}
		visibleZone = e.detail.items;
		// Cooldown MUST be set synchronously, before flipping `isDragging`,
		// so the resync `$effect` (which runs in a microtask before the
		// persist microtask) sees `dropCooldown=true` and skips clobbering
		// `visibleZone` back to the un-updated derived value. Without this
		// gate, the just-set post-drag order is reset before `persistGlobalOrder`
		// has a chance to fold it into `dndWorkspaces` — and the API call
		// then persists the pre-drag order, producing the visual snap-back.
		dropCooldown = true;
		dragArmed = false;
		isDragging = false;
		armDropClickGuard();
		schedulePersist();
	}

	function handleOverflowConsider(e: CustomEvent<DndEvent<Workspace>>) {
		overflowZone = e.detail.items;
		isDragging = true;
		dragRejected = false;
	}

	async function handleOverflowFinalize(e: CustomEvent<DndEvent<Workspace>>) {
		const next = e.detail.items;
		// Active-pin rule: if active landed in overflow, REJECT the entire
		// drag (no-op). Reset all zones from the pre-drag derived split
		// (which is computed from `dndWorkspaces` — un-mutated during
		// drag) so active stays at its original position. Don't persist.
		const activeSlug = workspaceStore.current?.slug;
		if (activeSlug && next.some((w) => w.slug === activeSlug)) {
			dragRejected = true;
			resetZonesFromProps();
			cancelPersist();
			// If the visible-zone finalize ran first, it set dropCooldown=true
			// and scheduled a persist. cancelPersist() above stops that persist,
			// but the cooldown timer was never armed (it's set inside the
			// cancelled microtask) — clear the cooldown now so sync effects
			// don't stay gated off forever.
			clearCooldownAfterRejection();
			dragArmed = false;
			isDragging = false;
			armDropClickGuard();
			return;
		}
		overflowZone = next;
		// Set cooldown BEFORE isDragging — see handleVisibleFinalize.
		dropCooldown = true;
		dragArmed = false;
		isDragging = false;
		armDropClickGuard();
		schedulePersist();
	}

	// Trigger zone — single-slot drop target. The menu auto-opens on
	// drag start (see the `.open` class binding on `.overflow-menu`), so
	// the older spring-loaded "hover for 400ms to open" behavior is no
	// longer needed. The trigger zone still exists so dropping directly
	// on the `…` button appends the item to the end of the overflow set.
	function handleTriggerConsider(e: CustomEvent<DndEvent<Workspace>>) {
		triggerZone = e.detail.items;
		isDragging = true;
		dragRejected = false;
	}

	async function handleTriggerFinalize(e: CustomEvent<DndEvent<Workspace>>) {
		const dropped = e.detail.items.filter(
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			(item: any) => !item[SHADOW_ITEM_MARKER_PROPERTY_NAME]
		);
		triggerZone = [];

		// Active-pin under "drop on trigger": REJECT the whole drag if
		// active was the dropped item, same as the overflow zone. Don't
		// silently strip it — that previously dropped active out of the
		// persisted order entirely (Codex round 1 HIGH).
		const activeSlug = workspaceStore.current?.slug;
		if (activeSlug && dropped.some((w) => w.slug === activeSlug)) {
			dragRejected = true;
			resetZonesFromProps();
			cancelPersist();
			// See clearCooldownAfterRejection comment in handleOverflowFinalize:
			// the source-zone finalize may have already set dropCooldown=true,
			// and the cancelled persist won't arm the cooldown timer.
			clearCooldownAfterRejection();
			dragArmed = false;
			isDragging = false;
			armDropClickGuard();
			return;
		}

		if (dropped.length > 0) {
			overflowZone = [...overflowZone, ...dropped];
		}
		// Set cooldown BEFORE isDragging — see handleVisibleFinalize.
		dropCooldown = true;
		dragArmed = false;
		isDragging = false;
		armDropClickGuard();
		schedulePersist();
	}

	// Both zones fire `finalize` for a cross-zone drag. Coalesce both into
	// a single persist call to avoid racing API writes for one user action.
	// `persistCancelled` is set when an active-pin rejection happens
	// between scheduling and the microtask running, so we skip the API
	// call entirely instead of racing with a corrupted order.
	let persistScheduled = false;
	let persistCancelled = false;
	// `dragRejected` flags an active-pin rejection so the OTHER zone's
	// finalize (which fires before or after the rejection — order isn't
	// guaranteed by svelte-dnd-action) doesn't clobber visibleZone with
	// its post-drag items, which exclude active.
	let dragRejected = false;
	function schedulePersist() {
		if (persistScheduled) return;
		// Clear any stale `persistCancelled` from a prior rejected drag.
		// In target-first finalize order, cancelPersist() can fire when
		// no microtask was queued (the source's schedulePersist hadn't
		// run yet). Without this clear, the flag would leak and silently
		// kill the next valid reorder. (Codex round 3 MEDIUM.)
		persistCancelled = false;
		persistScheduled = true;
		queueMicrotask(async () => {
			persistScheduled = false;
			if (persistCancelled) {
				persistCancelled = false;
				return;
			}
			await persistGlobalOrder();
		});
	}
	function cancelPersist() {
		persistCancelled = true;
	}

	// Reset both zones from the (un-mutated during drag) derived split.
	// Used when an entire drag must be rejected — the spec calls for
	// active-pin rejection to be a no-op, not a "snap back to end".
	function resetZonesFromProps() {
		visibleZone = [...propVisibleWorkspaces];
		overflowZone = [...propOverflowWorkspaces];
		triggerZone = [];
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
		// Cancel any pending cooldown clear from a previous persist BEFORE
		// awaiting — otherwise the prior timeout can fire mid-request,
		// flip dropCooldown false, and let SSE clobber the displayed order
		// while the new write is still in flight.
		if (cooldownTimer) {
			clearTimeout(cooldownTimer);
			cooldownTimer = null;
		}

		const updates = fullOrder.map((ws, i) => ({ slug: ws.slug, sort_order: i }));
		try {
			await api.workspaces.reorder(updates);
			await workspaceStore.loadAll();
			// Re-arm the cooldown clear after success.
			cooldownTimer = setTimeout(() => {
				dropCooldown = false;
				cooldownTimer = null;
			}, 1000);
		} catch {
			// Failure: restore from the un-reordered store and immediately
			// resync the displayed zones — the cooldown gate would
			// otherwise hide the rollback for up to a second.
			dndWorkspaces = [...workspaceStore.workspaces];
			visibleZone = [...propVisibleWorkspaces];
			overflowZone = [...propOverflowWorkspaces];
			if (cooldownTimer) {
				clearTimeout(cooldownTimer);
				cooldownTimer = null;
			}
			dropCooldown = false;
		}
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
	// Clear any pending timers when the component unmounts (e.g. layout
	// teardown, mobile-branch flip). Without this, a setTimeout that fires
	// after the component is gone would write to dead state — Svelte's
	// runtime no-ops the writes, but it's still tidier to cancel.
	$effect(() => {
		return () => {
			if (cooldownTimer) {
				clearTimeout(cooldownTimer);
				cooldownTimer = null;
			}
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
	function handleWsClick(e: MouseEvent, ws: Workspace) {
		// Suppress the synthetic click that fires after a drag finalizes
		// (see armDropClickGuard). Without this, every drop triggers a
		// goto() to the dragged workspace, refreshing the current page.
		if (dropClickGuard) {
			e.preventDefault();
			return;
		}
		if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
		e.preventDefault();
		closeOverflow();
		const target =
			ws.slug === currentSlug
				? `/${ws.owner_username}/${ws.slug}`
				: workspaceRestoreTarget(ws);
		goto(target);
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
	onmouseup={disarmMenu}
	onclick={(e) => {
		const target = e.target as HTMLElement;
		if (userMenuOpen && !target.closest('.user-menu-container')) {
			closeUserMenu();
		}
		// Don't close on outside-click during a drag — the click event
		// fired on mouseup at drag end would otherwise tear down the
		// menu while finalize handlers are still wiring up state.
		if (
			overflowOpen &&
			!isDragging &&
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
		<!--
			Centered row that holds the workspace pills, the overflow `…`
			trigger, and the `+` add button as a single visual group. The
			row is `flex: 1 1 auto; justify-content: center` so its
			contents stay centered as the topbar widens or narrows, and
			ResizeObserver measures THIS element to know how much space
			pills can claim (see CHROME_RESERVATION above). Putting the
			trigger + add inside this group is what keeps the `…` button
			right next to the last visible pill instead of pinned to the
			far right of the topbar.
		-->
		<div class="workspace-row" bind:this={rowEl}>
			<!-- svelte-ignore a11y_no_static_element_interactions -->
			<div
				class="workspace-list"
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
						middle-click / cmd-click / right-click → "Open in
						new tab" lands on a clean dashboard. Plain left-
						click is intercepted to restore the last-visited
						route in that workspace (TASK-754) — the previous
						behavior was a hard `<a>` nav to the dashboard,
						which silently overwrote the saved deep route in
						localStorage on every workspace switch.
					-->
					<a
						href="/{ws.owner_username}/{ws.slug}"
						class="workspace-item"
						class:active={ws.slug === currentSlug}
						title={ws.name}
						onmousedown={armMenuForDrag}
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
				Anchor box: a `position: relative` wrapper around the
				trigger and its dropdown menu. The menu is `position:
				absolute; right: 0` relative to this anchor, so it always
				opens directly under the `…` button instead of being
				centered against the topbar.
			-->
			<div class="overflow-anchor">
				<!--
					Overflow trigger. Always rendered (so layout stays
					stable as workspaces are added/removed) but visually
					hidden when nothing overflows. The wrapper is a 3rd
					dndzone with the same `type` as the visible row and
					the menu, so dropping on the trigger appends to
					overflow. Spring-loaded auto-open on hover-during-
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
						aria-expanded={menuVisible}
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

				<!--
					Always mount the menu DOM whenever overflow exists, so
					`svelte-dnd-action` can register its dndzone before any
					drag starts. (Mounting the zone mid-drag — what the old
					spring-load path did — leaves the in-progress drag
					unable to recognize the zone as a valid drop target.)
					Visual show/hide is controlled by the `.open` class:
					open whenever the user click-opened, a drag is in
					progress, OR the menu has been pre-armed by mousedown
					on a draggable pill. Pre-arming flips `pointer-events`
					to `auto` *before* `svelte-dnd-action`'s drop hit-test
					runs at drag start — see the dragArmed comment in the
					script section.
				-->
				{#if overflowZone.length > 0}
					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						id="workspace-overflow-menu"
						class="overflow-menu"
						class:open={menuVisible}
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
								onmousedown={armMenuForDrag}
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
			</div>

			<button
				class="workspace-add"
				onclick={() => uiStore.openCreateWorkspace()}
				title="New workspace"
			>
				<span class="add-icon">+</span>
			</button>
		</div>

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
								<button
									class="dropdown-item"
									onclick={() => {
										closeUserMenu();
										connectOpen = true;
									}}
								>
									Connect a project…
								</button>
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

						<!-- Resources — see desktop branch for placement rationale. -->
						<UserMenuResources cloudMode={authStore.cloudMode} onclose={closeUserMenu} />

						<!-- Connect a project — see desktop branch for placement rationale. -->
						{#if workspaceStore.current?.slug}
							<button
								class="dropdown-item"
								onclick={() => {
									closeUserMenu();
									connectOpen = true;
								}}
							>
								Connect a project…
							</button>
						{/if}
						<div class="dropdown-divider"></div>
						<button class="dropdown-item logout" onclick={handleLogout}>
							Sign out
						</button>
					</div>
				{/if}
			</div>
		{/if}

		<!-- Same Connect modal pattern as the desktop branch — see notes above. -->
		{#if workspaceStore.current?.slug}
			<ConnectWorkspaceModal
				bind:open={connectOpen}
				serverUrl={typeof window !== 'undefined' ? window.location.origin : ''}
				workspaceSlug={workspaceStore.current.slug}
				workspaceName={workspaceStore.current.name}
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
		Centered row that wraps the workspace pills, the … overflow
		trigger, and the + add button. `flex: 1 1 auto` claims the
		leftover topbar space so ResizeObserver sees the real available
		width (not the content-collapsed width of the inner pill list —
		that was the IDEA-758 regression fix). `justify-content: center`
		keeps the trio visually centered when there's slack, matching the
		pre-overflow-menu layout. Putting all three in this row is also
		what keeps the … button right after the last visible pill rather
		than pinned to the far edge of the topbar.
	*/
	.workspace-row {
		display: flex;
		align-items: center;
		justify-content: center;
		flex: 1 1 auto;
		gap: 2px;
		min-width: 0;
	}

	/*
		Workspace list — pills that fit. No more `overflow-x: auto`; pills
		that don't fit are routed into the .overflow-menu via
		propVisibleWorkspaces / propOverflowWorkspaces. See script for the
		measurement + split logic. Content-sized within `.workspace-row`
		so the … trigger sits immediately after the last visible pill.
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
	/*
		Truncate long workspace names inside the desktop bar (and the
		ghost row, so measurement and rendered widths match). Without this
		cap, a single long active-workspace name could blow past the bar
		because the active pill is pinned visible regardless of fit.
		Overflow menu rows aren't capped — full names read better there.
	*/
	.workspace-list .workspace-name,
	.workspace-ghost .workspace-name {
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

	/*
		Positioning anchor for the overflow trigger + dropdown menu.
		`position: relative` makes the absolutely-positioned `.overflow-
		menu` open directly under the `…` button (anchored to the right
		edge of this box) instead of being centered against the whole
		topbar. The anchor itself is a flex item inside `.workspace-row`
		and shrinks to its content (the trigger).
	*/
	.overflow-anchor {
		position: relative;
		display: flex;
		align-items: center;
		flex-shrink: 0;
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
		Overflow menu — anchored under the `…` trigger via `.overflow-
		anchor`'s `position: relative`. `right: 0` aligns the menu's
		right edge with the trigger's right edge, so the menu opens
		below right-aligned (per IDEA-758 spec) and extends leftward
		toward the pills.

		Mounted whenever `overflowZone.length > 0` so `svelte-dnd-
		action` registers the dndzone eagerly. Hidden via `visibility:
		hidden` when not `.open`. Subtle but critical: every other
		obvious "hide" mechanism breaks svelte-dnd-action's drop-zone
		hit-test (which uses `getBoundingRectNoTransforms` +
		`isPointInsideRect`, NOT `elementsFromPoint`):
		  • `display: none`              → rect is {0,0,0,0}; never hit.
		  • `pointer-events: none`       → no effect on the rect-based
		    hit-test, but sometimes interacts oddly with cursor capture.
		  • `transform: scale(0)`        → the library tries to undo
		    the transform mathematically (intersection.js:33-37), but
		    parses transform-origin with plain `parseFloat`, so a
		    percentage origin like `top right` ("100% 0%") is read as
		    "100 pixels", and the reconstructed rect lands far from
		    the visible menu position. The hit-test then always misses.
		`visibility: hidden` leaves `offsetWidth/Height` and
		`getBoundingClientRect` untouched, so svelte-dnd-action sees
		the correct rect at the menu's actual layout position whether
		the menu is visually shown or not — drops register reliably
		the moment the cursor enters the rect during a drag. Visibility
		also blocks pointer events and removes the element from the
		a11y tree when hidden, so closed-state clicks pass through to
		page content beneath cleanly. Opacity transitions for the
		visual fade-in; visibility flips instantly on `.open`.
	*/
	.overflow-menu {
		position: absolute;
		top: calc(100% + 6px);
		right: 0;
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
		visibility: hidden;
		opacity: 0;
		transition: opacity 0.12s ease-out;
	}
	.overflow-menu.open {
		visibility: visible;
		opacity: 1;
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
