import { browser } from '$app/environment';
import { viewport, MOBILE_MEDIA_QUERY } from '$lib/stores/breakpoint.svelte';

let sidebarOpen = $state(browser ? !viewport.isMobile : true);
let topbarOpen = $state(browser ? localStorage.getItem('pad-topbar') !== 'closed' : true);
let searchOpen = $state(false);
// The keyboard shortcuts sheet (TASK-2261): here rather than in the root layout
// so the account menu can open it, not only the `?` key.
let shortcutsOpen = $state(false);
let isTouch = $state(browser ? 'ontouchstart' in window : false);
// True while the on-screen keyboard is up. Detected from the geometry of the
// keyboard itself — visualViewport.height shrinking below the tallest height
// we've seen (the keyboard-closed baseline). This works on iOS Safari AND
// Android Chrome; a naive `innerHeight - visualViewport.height` does NOT,
// because those browsers shrink window.innerHeight in lockstep with the visual
// viewport, leaving the delta ~0. Consumers (e.g. BottomNav) hide fixed bottom
// chrome so it doesn't sit stranded above the keyboard. PLAN-1694.
let keyboardVisible = $state(false);
let createWorkspaceOpen = $state(false);
// Set when the last workspace tab is closed and the user lands on /console
// (PLAN-3002 Q2): the console page highlights its create button as the way
// back in. Cleared when that page unmounts or the create flow opens.
let addWorkspaceHighlighted = $state(false);
let quickAddRequested = $state(false);
let quickAddTargetSlug = $state<string | null>(null);
let collectionSearchHandler = $state<(() => void) | null>(null);
// Slug of a workspace whose Connect modal should auto-open as soon as the
// user lands on its workspace page. Set by CreateWorkspaceModal (via
// +layout.svelte's onWorkspaceCreated wire-up) right before `goto`; consumed
// by the workspace +page.svelte when its slug matches. Mirrors the
// request/clear pattern used by quickAdd above. PLAN-1519 / TASK-1526.
let connectAfterNavigateSlug = $state<string | null>(null);

if (browser) {
	// `isMobile` itself is owned by the shared breakpoint store (one app-wide
	// listener). Here we only run the layout side effects that must fire when
	// the viewport crosses the mobile breakpoint: collapse the sidebar entering
	// mobile, restore it leaving. `change` fires only on a crossing, so no
	// manual before/after comparison is needed.
	//
	// The collection page's split-pane detail view (PLAN-2105 / TASK-2121) is
	// deliberately NOT touched here: pane state is URL-derived (`?item=`), and
	// on mobile the pane simply restyles to a full-screen overlay via CSS (see
	// the `@media (max-width: 768px)` block in PaneHost). Closing it on a
	// crossing would silently drop `?item=` on every mobile entry, the exact
	// bug TASK-2121 avoids. (The legacy `detailPanelOpen` boolean this handler
	// used to force-close had no reader since the first release and was
	// removed with its ⌘] shortcut, BUG-2666.)
	window.matchMedia(MOBILE_MEDIA_QUERY).addEventListener('change', (e) => {
		if (e.matches) {
			sidebarOpen = false;
		} else {
			sidebarOpen = true;
		}
	});

	// Track on-screen keyboard visibility from the visual viewport. Only touch
	// devices raise a soft keyboard, so gate on isTouch — a narrow desktop
	// window never has one and must not hide the nav.
	if (isTouch && window.visualViewport) {
		const vv = window.visualViewport;
		// The tallest viewport height we've seen == keyboard closed. It grows as
		// the URL bar collapses on scroll and resets on rotation, so the keyboard
		// shrink is always measured against the true full-height reference.
		let baseline = vv.height;
		const KEYBOARD_MIN_PX = 150; // smaller shrinks are browser chrome, not a keyboard
		const measure = () => {
			if (vv.height > baseline) baseline = vv.height;
			keyboardVisible = baseline - vv.height > KEYBOARD_MIN_PX;
		};
		vv.addEventListener('resize', measure);
		vv.addEventListener('scroll', measure);
		// Re-capture the baseline after an orientation change so a shorter
		// landscape viewport isn't mistaken for an open keyboard.
		window.addEventListener('orientationchange', () => {
			baseline = 0;
			setTimeout(measure, 300);
		});
	}
}

export const uiStore = {
	get sidebarOpen() { return sidebarOpen; },
	get topbarOpen() { return topbarOpen; },
	get searchOpen() { return searchOpen; },
	get isMobile() { return viewport.isMobile; },
	get isTouch() { return isTouch; },
	get keyboardVisible() { return keyboardVisible; },
	get createWorkspaceOpen() { return createWorkspaceOpen; },

	toggleSidebar() { sidebarOpen = !sidebarOpen; },
	openSidebar() { sidebarOpen = true; },
	closeSidebar() { sidebarOpen = false; },

	toggleTopbar() {
		topbarOpen = !topbarOpen;
		if (browser) localStorage.setItem('pad-topbar', topbarOpen ? 'open' : 'closed');
	},
	openTopbar() {
		topbarOpen = true;
		if (browser) localStorage.setItem('pad-topbar', 'open');
	},
	closeTopbar() {
		topbarOpen = false;
		if (browser) localStorage.setItem('pad-topbar', 'closed');
	},
	/**
	 * Mod+\ (TASK-2261, audit C101): ONE chrome state for both bars. It used to
	 * toggle each independently, so with one bar hidden it showed that one and
	 * hid the other, and the two never lined up again. Now: if either bar is
	 * open, both close; if both are closed, both open. On mobile there is no
	 * workspace bar to show, so only the sidebar moves (the topbar's state is
	 * persisted, and flipping it unseen would surprise the desktop later).
	 */
	toggleChrome() {
		if (viewport.isMobile) {
			sidebarOpen = !sidebarOpen;
			return;
		}
		if (sidebarOpen || topbarOpen) {
			sidebarOpen = false;
			this.closeTopbar();
		} else {
			sidebarOpen = true;
			this.openTopbar();
		}
	},
	get shortcutsOpen() { return shortcutsOpen; },
	openShortcuts() { shortcutsOpen = true; },
	closeShortcuts() { shortcutsOpen = false; },
	toggleShortcuts() { shortcutsOpen = !shortcutsOpen; },
	openSearch() { searchOpen = true; },
	closeSearch() { searchOpen = false; },
	toggleSearch() { searchOpen = !searchOpen; },


	openCreateWorkspace() {
		createWorkspaceOpen = true;
		addWorkspaceHighlighted = false;
	},
	get addWorkspaceHighlighted() { return addWorkspaceHighlighted; },
	highlightAddWorkspace() { addWorkspaceHighlighted = true; },
	clearAddWorkspaceHighlight() { addWorkspaceHighlighted = false; },
	closeCreateWorkspace() { createWorkspaceOpen = false; },

	// Quick-add item trigger — sidebar watches this
	get quickAddRequested() { return quickAddRequested; },
	get quickAddTargetSlug() { return quickAddTargetSlug; },
	requestQuickAdd(collectionSlug?: string) { quickAddTargetSlug = collectionSlug ?? null; quickAddRequested = true; },
	clearQuickAddRequest() { quickAddRequested = false; quickAddTargetSlug = null; },

	// Connect-modal auto-open signal (PLAN-1519 / TASK-1526). Fired by
	// CreateWorkspaceModal after a successful create/import, consumed by
	// the workspace +page.svelte when the user lands on the matching
	// workspace. Always read via `consumeConnectAfterNavigate()` so the
	// signal is single-shot — re-reading the value would leave the request
	// stuck and reopen the modal on every reactive re-run.
	get connectAfterNavigateSlug() { return connectAfterNavigateSlug; },
	requestConnectAfterNavigate(slug: string) { connectAfterNavigateSlug = slug; },
	consumeConnectAfterNavigate(): string | null {
		const slug = connectAfterNavigateSlug;
		connectAfterNavigateSlug = null;
		return slug;
	},

	// Collection search (Cmd+F) registry — pages that want to handle Cmd+F
	// register a handler on mount and unregister on destroy. The layout's
	// global keydown handler only calls preventDefault when a handler is
	// registered, so on pages without one (e.g. item view) Cmd+F falls
	// through to the browser's native find. (BUG-986)
	get hasCollectionSearchHandler() { return collectionSearchHandler !== null; },
	triggerCollectionSearch() { collectionSearchHandler?.(); },
	registerCollectionSearch(handler: () => void) { collectionSearchHandler = handler; },
	unregisterCollectionSearch() { collectionSearchHandler = null; },

	/** Close sidebar on mobile after navigation */
	onNavigate() {
		if (viewport.isMobile) sidebarOpen = false;
	},
};
