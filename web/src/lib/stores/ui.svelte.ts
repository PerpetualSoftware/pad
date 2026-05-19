import { browser } from '$app/environment';

let sidebarOpen = $state(browser ? window.innerWidth > 768 : true);
let topbarOpen = $state(browser ? localStorage.getItem('pad-topbar') !== 'closed' : true);
let searchOpen = $state(false);
let isMobile = $state(browser ? window.innerWidth <= 768 : false);
let isTouch = $state(browser ? 'ontouchstart' in window : false);
let detailPanelOpen = $state(browser ? localStorage.getItem('pad-detail-panel') !== 'closed' && window.innerWidth > 768 : false);
let createWorkspaceOpen = $state(false);
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
	window.addEventListener('resize', () => {
		const mobile = window.innerWidth <= 768;
		if (mobile !== isMobile) {
			isMobile = mobile;
			if (mobile) {
				sidebarOpen = false;
				detailPanelOpen = false;
			} else {
				sidebarOpen = true;
			}
		}
	});
}

export const uiStore = {
	get sidebarOpen() { return sidebarOpen; },
	get topbarOpen() { return topbarOpen; },
	get searchOpen() { return searchOpen; },
	get isMobile() { return isMobile; },
	get isTouch() { return isTouch; },
	get detailPanelOpen() { return detailPanelOpen; },
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
	openSearch() { searchOpen = true; },
	closeSearch() { searchOpen = false; },
	toggleSearch() { searchOpen = !searchOpen; },

	toggleDetailPanel() {
		detailPanelOpen = !detailPanelOpen;
		if (browser) localStorage.setItem('pad-detail-panel', detailPanelOpen ? 'open' : 'closed');
	},
	openDetailPanel() {
		detailPanelOpen = true;
		if (browser) localStorage.setItem('pad-detail-panel', 'open');
	},
	closeDetailPanel() {
		detailPanelOpen = false;
		if (browser) localStorage.setItem('pad-detail-panel', 'closed');
	},

	openCreateWorkspace() { createWorkspaceOpen = true; },
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
		if (isMobile) sidebarOpen = false;
	},
};
