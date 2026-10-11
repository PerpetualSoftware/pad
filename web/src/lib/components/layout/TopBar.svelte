<script lang="ts">
	import { confirmSignOut, signOutFailed } from '$lib/stores/signOutGuard.svelte';
	import { modKeyLabel } from '$lib/utils/platform';
	import { dndzone, SHADOW_ITEM_MARKER_PROPERTY_NAME } from 'svelte-dnd-action';
	import type { DndEvent } from 'svelte-dnd-action';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { tabsStore } from '$lib/stores/tabs.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { api } from '$lib/api/client';
	import type { WorkspaceTab } from '$lib/types';
	import { onMount, tick, untrack } from 'svelte';
	import { afterNavigate, goto } from '$app/navigation';
	import PadLogo from '$lib/components/layout/PadLogo.svelte';
	import WorkspaceSwitcher from '$lib/components/layout/WorkspaceSwitcher.svelte';
	import UserMenuResources from '$lib/components/layout/UserMenuResources.svelte';
	import ConnectWorkspaceModal from '$lib/components/ConnectWorkspaceModal.svelte';
	import WorkspaceDiscovery from '$lib/components/layout/WorkspaceDiscovery.svelte';
	import Menu from '$lib/components/common/Menu.svelte';
	import MenuItem from '$lib/components/common/MenuItem.svelte';
	import { workspaceRestoreTarget } from '$lib/utils/workspace-route';
	import { tabLanding } from '$lib/utils/tabLanding';
	import { pendingInvitations } from '$lib/stores/pendingInvitations.svelte';

	let { mobile = false }: { mobile?: boolean } = $props();

	let userMenuOpen = $state(false);
	let userTriggerEl: HTMLButtonElement | undefined = $state(undefined);
	let currentTheme = $state<'dark' | 'light'>('dark');
	let connectOpen = $state(false);
	// The "+" discovery surface (PLAN-3002 U4): find a workspace that is not
	// open as a tab, create one, or restore a deleted one.
	let discoveryOpen = $state(false);
	let addEl: HTMLButtonElement | undefined = $state(undefined);

	// Pending invitations badge on "+" (BUG-2136 U2). Refetched on mount,
	// window focus and every navigation, so an invitee sees a new invitation
	// on their next page view; the store throttles the focus and navigation
	// refetches, so a burst of either costs one request.
	const invitationCount = $derived(pendingInvitations.count);
	const addLabel = $derived(
		invitationCount > 0
			? `Find or create a workspace (${invitationCount} pending ${invitationCount === 1 ? 'invitation' : 'invitations'})`
			: 'Find or create a workspace'
	);
	onMount(() => {
		// A mount is a page load: it skips the throttle, so a remount within the
		// window still fetches.
		void pendingInvitations.refresh(true);
		const onFocus = () => void pendingInvitations.refresh();
		window.addEventListener('focus', onFocus);
		return () => window.removeEventListener('focus', onFocus);
	});
	afterNavigate(() => void pendingInvitations.refresh());

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

	// ── Chrome feel (TASK-3312) ──────────────────────────────────────────
	// Motion is CSS classes, never Svelte in:/out: transitions: the strip is a
	// dndzone, and an outgoing element that lingers in the DOM would be counted
	// among the zone's items.
	const OPEN_MS = 160;
	const CLOSE_MS = 140;
	/** At or above this width every tab shows its ×; below, hover or focus does. */
	const WIDE_TAB_PX = 150;

	let reduceMotion = $state(false);
	let opening = $state(new Set<string>());
	let closing = $state(new Set<string>());
	/** Width of the tab at the moment its close began, for the collapse keyframe. */
	let closingFrom = $state<Record<string, number>>({});
	let narrow = $state(false);

	// Tabs that appear after the first load grow in. The first list is the
	// page arriving, not a tab opening, so it does not animate.
	let seenSlugs: Set<string> | null = null;
	$effect(() => {
		const slugs = tabsStore.tabs.map((t) => t.slug);
		if (!tabsStore.loaded) return;
		if (seenSlugs === null) {
			seenSlugs = new Set(slugs);
			return;
		}
		const fresh = slugs.filter((s) => !seenSlugs!.has(s));
		seenSlugs = new Set(slugs);
		if (fresh.length === 0 || untrack(() => reduceMotion)) return;
		// Read through untrack: this effect writes `opening`, and a read here
		// would make it depend on its own write.
		opening = new Set([...untrack(() => opening), ...fresh]);
		setTimeout(() => {
			const next = new Set(opening);
			for (const s of fresh) next.delete(s);
			opening = next;
		}, OPEN_MS);
	});

	// Close-freeze (Chrome): after a close from this strip, the remaining tabs
	// keep the widths they had, so the next × lands under a pointer that did
	// not move and repeated closes need no aiming. Released when the pointer
	// leaves the strip, on a window resize, and on any tab change that is not
	// this close settling (an open elsewhere, the access stream): a frozen
	// strip must never hold widths for a tab set it was not frozen for.
	let frozen = $state<Record<string, number> | null>(null);
	let frozenFor: { before: string; after: string } | null = null;

	function freezeWidths(except: string) {
		if (!listEl) return;
		const widths: Record<string, number> = {};
		for (const el of listEl.querySelectorAll<HTMLElement>('.workspace-tab')) {
			const slug = el.dataset.wsSlug;
			if (slug && slug !== except) widths[slug] = el.getBoundingClientRect().width;
		}
		const slugs = tabsStore.tabs.map((t) => t.slug);
		frozenFor = { before: slugs.join('\n'), after: slugs.filter((s) => s !== except).join('\n') };
		frozen = widths;
	}
	function releaseFreeze() {
		frozen = null;
		frozenFor = null;
	}
	$effect(() => {
		const key = tabsStore.tabs.map((t) => t.slug).join('\n');
		if (frozenFor && key !== frozenFor.before && key !== frozenFor.after) releaseFreeze();
	});

	onMount(() => {
		const motion = window.matchMedia('(prefers-reduced-motion: reduce)');
		reduceMotion = motion.matches;
		const onMotion = (e: MediaQueryListEvent) => (reduceMotion = e.matches);
		motion.addEventListener('change', onMotion);
		window.addEventListener('resize', releaseFreeze);
		// One observer for the × rule: every tab shares one width, so the
		// first tab's width answers for all of them.
		const measure = () => {
			const first = listEl?.querySelector<HTMLElement>('.workspace-tab:not(.closing)');
			narrow = !!first && first.getBoundingClientRect().width < WIDE_TAB_PX;
		};
		const ro = new ResizeObserver(measure);
		const watch = () => {
			ro.disconnect();
			if (!listEl) return;
			ro.observe(listEl);
			for (const el of listEl.querySelectorAll('.workspace-tab')) ro.observe(el);
		};
		watch();
		const mo = new MutationObserver(watch);
		if (listEl) mo.observe(listEl, { childList: true });
		return () => {
			motion.removeEventListener('change', onMotion);
			window.removeEventListener('resize', releaseFreeze);
			ro.disconnect();
			mo.disconnect();
		};
	});

	// Middle-click closes a tab, Chrome's rule (TASK-3312). Kept to these two
	// handlers so the gesture can be moved (to Ctrl/Cmd+click, say) without
	// touching the close itself. mousedown's default is the autoscroll cursor;
	// auxclick's is "open link in a new tab", which closeTab prevents.
	function handleTabMouseDown(e: MouseEvent) {
		if (e.button === 1) e.preventDefault();
	}
	function handleTabAuxClick(e: MouseEvent, tab: WorkspaceTab) {
		if (e.button !== 1) return;
		void closeTab(e, tab);
	}

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

	// ── Tab strip: overflow cues, active tab in view, roving focus (TASK-3306) ──
	// Tabs shrink to a minimum width; past it the list scrolls. The hidden
	// scrollbar gave no cue, so a fade marks each side with tabs out of view,
	// and the active tab is scrolled into view whenever it or the set changes.
	let listEl: HTMLDivElement | undefined = $state(undefined);
	let fadeLeft = $state(false);
	let fadeRight = $state(false);
	function updateFades() {
		if (!listEl) return;
		const { scrollLeft, scrollWidth, clientWidth } = listEl;
		fadeLeft = scrollLeft > 1;
		fadeRight = scrollLeft + clientWidth < scrollWidth - 1;
	}
	$effect(() => {
		if (!listEl || typeof ResizeObserver === 'undefined') return;
		const ro = new ResizeObserver(updateFades);
		ro.observe(listEl);
		return () => ro.disconnect();
	});
	// The fade width, so a tab brought into view is not left under a fade.
	const FADE_PX = 24;
	$effect(() => {
		const slug = currentSlug;
		void dndTabs.length;
		if (!listEl || !slug || isDragging) return;
		const el = listEl.querySelector<HTMLElement>(`.workspace-tab[data-ws-slug="${CSS.escape(slug)}"]`);
		if (el) {
			// Scroll the LIST only: scrollIntoView would also move the page.
			const left = el.offsetLeft;
			const right = left + el.offsetWidth;
			if (left < listEl.scrollLeft + FADE_PX) listEl.scrollLeft = Math.max(0, left - FADE_PX);
			else if (right > listEl.scrollLeft + listEl.clientWidth - FADE_PX)
				listEl.scrollLeft = right - listEl.clientWidth + FADE_PX;
		}
		updateFades();
	});

	// Roving focus: one tab link is in the Tab order (the one last focused,
	// else the active tab, else the first), and the arrow keys, Home and End
	// move between tabs. The close button of that tab is the next Tab stop;
	// the others' close buttons are reachable by moving to their tab.
	let rovingSlug = $state('');
	let focusSlug = $derived(
		dndTabs.some((t) => t.slug === rovingSlug)
			? rovingSlug
			: dndTabs.some((t) => t.slug === currentSlug)
				? currentSlug
				: (dndTabs[0]?.slug ?? '')
	);
	// Ctrl+Shift+Left/Right moves the focused tab: the keyboard's reorder,
	// since the zone's own keyboard drag needed the tab wrapper focusable,
	// which put a second Tab stop on every tab (TASK-3306, codex r1). It goes
	// through the same store write a drag does, and like a drag it keeps an
	// ephemeral tab (PLAN-3002 Q9). Focus follows the moved tab.
	// A move applies locally at once and writes through tabsStore.reorder,
	// which sends reorders in call order (two quick presses used to race two
	// PUTs, and the server could store the older order: measured 2 in 8 on
	// the e2e with the writes unchained). The counter holds the store-to-zone
	// sync off until the last move's write settles.
	let pendingMoves = 0;
	function moveTab(index: number, delta: number) {
		const to = index + delta;
		if (to < 0 || to >= dndTabs.length || isDragging) return;
		const items = dndTabs.slice();
		const [moved] = items.splice(index, 1);
		items.splice(to, 0, moved);
		dndTabs = items;
		rovingSlug = moved.slug;
		pendingMoves++;
		persisting = true;
		void tick().then(() => listEl?.querySelectorAll<HTMLElement>('.workspace-item')[to]?.focus());
		void (async () => {
			try {
				await tabsStore.reorder(items.map((t) => t.slug));
				if (moved.ephemeral) await tabsStore.pin(moved.slug);
			} catch {
				// The store kept its last committed order; the sync shows it.
			} finally {
				pendingMoves--;
				if (pendingMoves === 0) persisting = false;
			}
		})();
	}

	function handleTabKeydown(e: KeyboardEvent, index: number) {
		if (e.ctrlKey && e.shiftKey && !e.altKey && !e.metaKey && (e.key === 'ArrowLeft' || e.key === 'ArrowRight')) {
			e.preventDefault();
			e.stopPropagation();
			moveTab(index, e.key === 'ArrowLeft' ? -1 : 1);
			return;
		}
		if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
		let next = -1;
		if (e.key === 'ArrowRight') next = Math.min(dndTabs.length - 1, index + 1);
		else if (e.key === 'ArrowLeft') next = Math.max(0, index - 1);
		else if (e.key === 'Home') next = 0;
		else if (e.key === 'End') next = dndTabs.length - 1;
		if (next < 0) return;
		e.preventDefault();
		e.stopPropagation();
		rovingSlug = dndTabs[next].slug;
		listEl?.querySelectorAll<HTMLElement>('.workspace-item')[next]?.focus();
	}

	// Double-click keeps an ephemeral tab (PLAN-3002 Q9). The clicks of a
	// double-click still reach handleWsClick; its detail check stops the
	// second one from navigating again.
	function handleTabDblClick(tab: WorkspaceTab) {
		if (tab.ephemeral) void tabsStore.pin(tab.slug).catch(() => {});
	}

	function keepTab(e: MouseEvent, tab: WorkspaceTab) {
		e.preventDefault();
		e.stopPropagation();
		void tabsStore.pin(tab.slug).catch(() => {});
	}

	// Closing a tab (PLAN-3002 Q2, Q3). Only closing the ACTIVE tab moves you,
	// to where tabLanding says (the one copy of the Q3 rule, shared with the
	// lost-workspace path; TASK-3280), at that tab's last route. Closing the
	// last tab lands on /console with "+" highlighted as the way back in.
	async function closeTab(e: MouseEvent, tab: WorkspaceTab) {
		e.preventDefault();
		e.stopPropagation();
		if (closing.has(tab.slug)) return;
		freezeWidths(tab.slug);
		if (!reduceMotion) {
			const el = listEl?.querySelector<HTMLElement>(`.workspace-tab[data-ws-slug="${CSS.escape(tab.slug)}"]`);
			closingFrom = { ...closingFrom, [tab.slug]: el?.getBoundingClientRect().width ?? 0 };
			closing = new Set([...closing, tab.slug]);
			await new Promise((r) => setTimeout(r, CLOSE_MS));
		}
		const before = tabsStore.tabs.slice();
		const wasActive = tab.slug === currentSlug;
		try {
			await tabsStore.close(tab.slug);
		} catch {
			return;
		} finally {
			if (closing.has(tab.slug)) {
				const next = new Set(closing);
				next.delete(tab.slug);
				closing = next;
			}
		}
		if (!wasActive) return;
		const landing = tabLanding(before, tab.slug, tabsStore.tabs);
		if (!landing) {
			// Set once the navigation has landed: /console has no TopBar, so
			// the console page shows it on its own create button and clears it
			// when it unmounts.
			await goto('/console');
			uiStore.highlightAddWorkspace();
			return;
		}
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
		// BUG-3571: unsaved edits in an open item are asked about first.
		if (!(await confirmSignOut())) return;
		try {
			await api.auth.logout();
			window.location.href = '/login';
		} catch {
			signOutFailed();
		}
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
					<!-- TASK-2255 (C59): not the workspace's ⚙ Settings. -->
					<a href="/console/settings" class="menu-link" role="menuitem" onclick={closeUserMenu}>
						Account settings
					</a>
					<!-- Hidden in the mobile apps: no purchase path there (PLAN-3291 DR-3). -->
					{#if authStore.commerceAllowed}
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
					<!-- The sheet's one entry point used to be the undocumented `?`
					     key (TASK-2261, audit C102). -->
					<MenuItem
						onclick={() => {
							closeUserMenu();
							uiStore.openShortcuts();
						}}
					>
						Keyboard shortcuts
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
				class:fade-left={fadeLeft}
				class:fade-right={fadeRight}
				class:narrow
				bind:this={listEl}
				onscroll={updateFades}
				onpointerleave={releaseFreeze}
				use:dndzone={{
					items: dndTabs,
					flipDurationMs,
					type: 'topbar-workspace',
					dragDisabled: uiStore.isTouch,
					// The zone and its items default to tabindex 0, which put
					// the list and every tab wrapper in the Tab order ahead of
					// the roving tab link (TASK-3306, codex r1). The zone's
					// keyboard drag started only from a focused wrapper
					// (Space on the link does not reach it, measured), so
					// Ctrl+Shift+Left/Right on the link (moveTab) replaces it.
					zoneTabIndex: -1,
					zoneItemTabIndex: -1
				}}
				onconsider={handleTabsConsider}
				onfinalize={handleTabsFinalize}
			>
				{#each dndTabs as tab, i (tab.id)}
					<!-- svelte-ignore a11y_no_static_element_interactions -->
					<div
						class="workspace-tab"
						class:active={tab.slug === currentSlug}
						class:ephemeral={tab.ephemeral}
						class:guest={tab.is_guest}
						class:opening={opening.has(tab.slug)}
						class:closing={closing.has(tab.slug)}
						class:frozen={!!frozen?.[tab.slug]}
						style:width={frozen?.[tab.slug] ? `${frozen[tab.slug]}px` : undefined}
						style:--closing-from={closing.has(tab.slug) ? `${closingFrom[tab.slug]}px` : undefined}
						data-ws-slug={tab.slug}
						onfocusin={() => (rovingSlug = tab.slug)}
						onmousedown={handleTabMouseDown}
						onauxclick={(e) => handleTabAuxClick(e, tab)}
					>
						<a
							href="/{tab.owner_username}/{tab.slug}"
							class="workspace-item"
							class:active={tab.slug === currentSlug}
							title={tab.is_guest ? `${tab.name} (shared with you)` : tab.name}
							aria-current={tab.slug === currentSlug ? 'page' : undefined}
							tabindex={tab.slug === focusSlug ? 0 : -1}
							aria-keyshortcuts="Control+Shift+ArrowLeft Control+Shift+ArrowRight"
							onkeydown={(e) => handleTabKeydown(e, i)}
							onclick={(e) => handleWsClick(e, tab)}
							ondblclick={() => handleTabDblClick(tab)}
						>
							<!-- A favicon-like filled square in both states (TASK-3312); an
							     inactive tab's is the same colour, quieter. A ring filled
							     only when active read as an avatar. -->
							<span
								class="workspace-icon"
								style:background={tab.slug === currentSlug
									? wsColor(tab.name)
									: `color-mix(in srgb, ${wsColor(tab.name)} 55%, var(--bg-primary))`}
							>
								{wsInitial(tab.name)}
							</span>
							<span class="workspace-name">{tab.name}</span>
						</a>
						{#if tab.ephemeral}
							<!-- "Keep open" (PLAN-3002 Q9): the explicit way to keep a
							     tab a landing opened, beside double-click and drag. -->
							<button
								type="button"
								class="workspace-tab-close workspace-tab-keep"
								aria-label="Keep {tab.name} open"
								tabindex={tab.slug === focusSlug ? 0 : -1}
								title="Keep open"
								onclick={(e) => keepTab(e, tab)}
							>
								<svg aria-hidden="true" width="11" height="11" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M6 2h4l-.5 4 2.5 2.5v1H4v-1L6.5 6z" /><path d="M8 9.5V14" /></svg>
							</button>
						{/if}
						<button
							type="button"
							class="workspace-tab-close"
							aria-label="Close {tab.name}"
							tabindex={tab.slug === focusSlug ? 0 : -1}
							title="Close"
							onclick={(e) => closeTab(e, tab)}
						>
							<span aria-hidden="true">×</span>
						</button>
					</div>
				{/each}
			</div>

			<div class="workspace-add-anchor">
				<button
					class="workspace-add"
					bind:this={addEl}
					onclick={() => (discoveryOpen = !discoveryOpen)}
					title={addLabel}
					aria-label={addLabel}
					aria-haspopup="listbox"
					aria-expanded={discoveryOpen}
				>
					<span class="add-icon">+</span>
					{#if invitationCount > 0}
						<span class="add-badge" data-testid="invitation-badge" aria-hidden="true">{invitationCount}</span>
					{/if}
				</button>
				<WorkspaceDiscovery open={discoveryOpen} onclose={() => (discoveryOpen = false)} trigger={addEl} />
			</div>
		</div>

		<div class="topbar-right">
			<button
				class="collapse-btn"
				onclick={() => uiStore.closeTopbar()}
				title="Hide workspace bar ({modKeyLabel('\\')} hides both bars)"
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
				mcpAuth={authStore.mcpAuth}
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
				mcpAuth={authStore.mcpAuth}
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

	/* Desktop: the tab strip starts at the left, after the logo, and stops
	   short of the collapse button and the avatar (TASK-3306; 72px let the
	   row run under both). */
	.topbar:not(.topbar-mobile) {
		justify-content: flex-start;
		padding: 0 108px 0 var(--space-3);
	}

	/* The logo sits IN the row on desktop, so the first tab starts a fixed
	   distance after it: the bar's 8px gap plus the tab list's 9px inset
	   (room for the active tab's flare), about 17px. It was
	   absolutely placed with the strip at a fixed 72px, so the space was
	   whatever the wordmark's width left over, which depends on the system
	   font: about 7px on one machine, about 1px on CI's (TASK-3545). */
	.topbar:not(.topbar-mobile) .topbar-left {
		position: static;
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
		The workspace tab bar (PLAN-3002 U3, reshaped as tabs by TASK-3306):
		the tab list and "+" as one left-aligned group, with "+" right after
		the last tab. The row stretches to the bar's full height so a tab can
		sit on the bar's bottom border. `min-width: 0` lets it shrink below its
		content so the list scrolls instead of pushing the user menu off.
	*/
	.workspace-row {
		display: flex;
		align-self: stretch;
		align-items: flex-end;
		justify-content: flex-start;
		flex: 1 1 auto;
		gap: 4px;
		min-width: 0;
	}

	/* One zone, plain horizontal scroll. The scrollbar is hidden: the bar is
	   a single row of chrome and scrolls by wheel, trackpad and drag. */
	.workspace-list {
		position: relative; /* offsetLeft of a tab is measured from here */
		display: flex;
		align-self: stretch;
		/* The LIST reaches over the bar's 1px bottom border, so the active tab
		   (and its flares) paint over it and join the page (TASK-3312). A tab's
		   own negative margin could not: overflow-x: auto makes the list clip
		   vertically too, so the border always showed under the active tab. */
		margin-bottom: -1px;
		/* Room for the active tab's 8px flares at either end: the list clips
		   what overflows it, so a first or last active tab lost its outer
		   flare (TASK-3312). */
		padding: 0 9px;
		align-items: flex-end;
		gap: 2px;
		min-width: 0;
		max-width: 100%;
		overflow-x: auto;
		scrollbar-width: none;
	}
	/* Past the minimum width the list scrolls; a fade marks each side that
	   has tabs out of view, since the scrollbar is hidden. */
	.workspace-list.fade-right {
		mask-image: linear-gradient(to right, #000 calc(100% - 24px), transparent);
	}
	.workspace-list.fade-left {
		mask-image: linear-gradient(to left, #000 calc(100% - 24px), transparent);
	}
	.workspace-list.fade-left.fade-right {
		mask-image: linear-gradient(to right, transparent, #000 24px, #000 calc(100% - 24px), transparent);
	}
	.workspace-list::-webkit-scrollbar {
		display: none;
	}

	/* A tab (TASK-3306): the TAB carries the shape and the background, so its
	   close button sits inside it. 200px while there is room; every tab
	   shrinks equally as more open, down to 120px (the icon, about six
	   characters and the X), and past that the list scrolls. The width is a
	   `width`, not a flex-basis, because a flex container sizes itself from
	   its items' content: with a basis alone the list took the tabs' content
	   width and they shrank with room to spare. The active tab takes the
	   page's background and overlaps the bar's bottom border, so it joins the
	   content below. The close button shows on hover, on keyboard focus within
	   the tab, and always on the active tab. */
	.workspace-tab {
		position: relative;
		display: flex;
		align-items: center;
		flex: 0 1 auto;
		width: 200px;
		min-width: 120px;
		/* 38px in the 44px bar (TASK-3312; 34 read short, with dead space above). */
		height: 38px;
		padding: 0 4px 0 2px;
		border: 1px solid transparent;
		border-bottom: none;
		border-radius: 8px 8px 0 0;
		transition: background 0.15s, width 0.15s;
	}
	/* A frozen tab keeps the width written on it (the close-freeze). */
	.workspace-tab.frozen {
		flex-shrink: 0;
	}

	/* Motion (TASK-3312): a new tab grows in, a closing one collapses. min-width
	   moves with max-width, since a min-width wins over any max-width. */
	.workspace-tab.opening {
		animation: tab-open 160ms ease-out;
	}
	@keyframes tab-open {
		from {
			min-width: 0;
			max-width: 0;
			opacity: 0;
		}
		to {
			min-width: 120px;
			max-width: 200px;
			opacity: 1;
		}
	}
	.workspace-tab.closing {
		animation: tab-close 140ms ease-in forwards;
		pointer-events: none;
	}
	@keyframes tab-close {
		from {
			min-width: var(--closing-from);
			max-width: var(--closing-from);
		}
		to {
			min-width: 0;
			max-width: 0;
			padding: 0;
			opacity: 0;
		}
	}
	@media (prefers-reduced-motion: reduce) {
		.workspace-tab {
			transition: background 0.15s;
		}
		.workspace-tab.opening,
		.workspace-tab.closing {
			animation: none;
		}
	}

	/* Dividers between inactive tabs (TASK-3312), Chrome's rule: none beside
	   the hovered or the active tab, and none after the last. The active tab's
	   pseudo-elements are its flares, so the divider is inactive-only. */
	.workspace-tab:not(.active)::after {
		content: '';
		position: absolute;
		right: -2px;
		top: 50%;
		width: 1px;
		height: 18px;
		transform: translateY(-50%);
		background: var(--border);
		pointer-events: none;
	}
	.workspace-tab:not(.active):hover::after,
	.workspace-tab:not(.active):has(+ .workspace-tab:hover)::after,
	.workspace-tab:not(.active):has(+ .workspace-tab.active)::after,
	.workspace-tab:not(.active):last-child::after {
		display: none;
	}

	/* Active-tab flare (TASK-3312): outward curves at the bottom corners, so the
	   tab flows into the page. Each is an 8px square outside the tab, filled
	   with the page background and cut by a quarter circle whose rim carries
	   the border. Tokens, so both themes follow. */
	.workspace-tab.active::before,
	.workspace-tab.active::after {
		content: '';
		position: absolute;
		bottom: 0;
		width: 8px;
		height: 8px;
		pointer-events: none;
	}
	.workspace-tab.active::before {
		left: -9px;
		background: radial-gradient(
			circle at 0 0,
			transparent 7.5px,
			var(--border) 7.5px,
			var(--border) 8.5px,
			var(--bg-primary) 8.5px
		);
	}
	.workspace-tab.active::after {
		right: -9px;
		background: radial-gradient(
			circle at 100% 0,
			transparent 7.5px,
			var(--border) 7.5px,
			var(--border) 8.5px,
			var(--bg-primary) 8.5px
		);
	}
	.workspace-tab:hover {
		background: var(--bg-hover);
	}
	/* The selected tab carries a 2px accent line along its top edge (BUG-3420).
	   Its background alone could not mark it: the active tab flows into the
	   page, so it IS the page colour, and that measured 1.07:1 (dark) and
	   1.09:1 (light) against the strip and its neighbours, with the same text
	   colour and weight. The line is WCAG non-text contrast against both: in
	   dark #9268f8 is 4.85:1 on the strip and 5.20:1 on the tab, in light
	   #7c3aed is 5.70:1 and 5.24:1. Inset, so it follows the rounded corners
	   and moves nothing. */
	.workspace-tab.active {
		background: var(--bg-primary);
		border-color: var(--border);
		box-shadow: inset 0 2px 0 var(--accent-primary);
		z-index: 1;
	}
	.workspace-tab-close {
		display: flex;
		align-items: center;
		justify-content: center;
		flex-shrink: 0;
		width: 18px;
		height: 18px;
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
	/* The × shows on every tab while tabs are wide (TASK-3312, Chrome's rule);
	   once they are narrow, on hover, keyboard focus and the active tab only.
	   The Keep-open pin keeps the reveal-only rule at every width. */
	.workspace-list:not(.narrow) .workspace-tab-close:not(.workspace-tab-keep),
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
		flex: 1 1 auto;
		min-width: 0;
		gap: var(--space-2);
		padding: var(--space-1) 6px;
		border-radius: var(--radius);
		text-decoration: none;
		color: var(--text-secondary);
		white-space: nowrap;
		transition: color 0.15s;
	}
	/* The arrow, like Chrome's tabs (TASK-3312). It was grab/grabbing, which
	   told every click it was a drag; drag works the same without it. */
	.topbar:not(.topbar-mobile) .workspace-item {
		cursor: default;
	}
	.workspace-item:hover {
		color: var(--text-primary);
		text-decoration: none;
	}
	.workspace-item.active {
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

	/* In a tab the icon is a favicon-like rounded square, filled in both
	   states (TASK-3312); the colour is set inline. The initial is NOT white:
	   white on these palette colours measured 1.67-2.75:1 on the full colour
	   and 1.39-1.79:1 on the light-mode inactive mix. An inactive icon takes
	   --text-primary (#191922 on the light mix, #f0f0f4 on the dark mix:
	   9.74:1 and 4.19:1 at worst), and an active one, on the full colour in
	   either theme, a fixed dark ink (6.34:1 at worst). */
	.workspace-tab .workspace-icon {
		width: 18px;
		height: 18px;
		border: none;
		border-radius: 4px;
		color: var(--text-primary);
		font-size: 0.66em;
	}
	.workspace-tab.active .workspace-icon {
		color: #191922;
	}

	.workspace-name {
		font-size: 0.82em;
		font-weight: 500;
	}
	/* A long name ends in an ellipsis inside its tab; the link's title
	   carries the full name. */
	.workspace-list .workspace-name {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.workspace-add-anchor {
		position: relative;
		align-self: center;
		flex-shrink: 0;
	}
	.workspace-add {
		position: relative;
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
	/* Pending invitations on "+" (BUG-2136 U2): visible without opening it. */
	.add-badge {
		position: absolute;
		top: -6px;
		right: -8px;
		min-width: 16px;
		height: 16px;
		padding: 0 4px;
		border-radius: 8px;
		background: var(--accent-blue);
		color: var(--bg-primary);
		font-size: 10px;
		font-weight: 600;
		line-height: 16px;
		text-align: center;
		pointer-events: none;
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
