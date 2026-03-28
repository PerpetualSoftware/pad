import { browser } from '$app/environment';

let sidebarOpen = $state(browser ? window.innerWidth > 768 : true);
let searchOpen = $state(false);
let isMobile = $state(browser ? window.innerWidth <= 768 : false);
let isTouch = $state(browser ? 'ontouchstart' in window : false);
let detailPanelOpen = $state(browser ? localStorage.getItem('pad-detail-panel') !== 'closed' && window.innerWidth > 768 : false);
let createWorkspaceOpen = $state(false);

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
	get searchOpen() { return searchOpen; },
	get isMobile() { return isMobile; },
	get isTouch() { return isTouch; },
	get detailPanelOpen() { return detailPanelOpen; },
	get createWorkspaceOpen() { return createWorkspaceOpen; },

	toggleSidebar() { sidebarOpen = !sidebarOpen; },
	openSidebar() { sidebarOpen = true; },
	closeSidebar() { sidebarOpen = false; },
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

	/** Close sidebar on mobile after navigation */
	onNavigate() {
		if (isMobile) sidebarOpen = false;
	},
};
