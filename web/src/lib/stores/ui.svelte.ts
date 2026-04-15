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
let collectionSearchRequested = $state(false);

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

	// Collection search (Cmd+F) trigger — collection page watches this
	get collectionSearchRequested() { return collectionSearchRequested; },
	requestCollectionSearch() { collectionSearchRequested = true; },
	clearCollectionSearchRequest() { collectionSearchRequested = false; },

	/** Close sidebar on mobile after navigation */
	onNavigate() {
		if (isMobile) sidebarOpen = false;
	},
};
