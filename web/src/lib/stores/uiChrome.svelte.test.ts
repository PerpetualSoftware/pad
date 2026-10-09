// TASK-2261 (audit C101): Mod+\ is ONE chrome state. It used to toggle the
// sidebar and the workspace bar independently, so with one hidden it showed
// that one and hid the other, and the two never lined up again.
import { describe, expect, it, vi, beforeEach } from 'vitest';

const vp = vi.hoisted(() => ({ mobile: false }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: {
		get isMobile() {
			return vp.mobile;
		},
		get isCoarsePointer() {
			return false;
		}
	},
	MOBILE_MEDIA_QUERY: '(max-width: 768px)'
}));

import { uiStore } from './ui.svelte';

function bars() {
	return { sidebar: uiStore.sidebarOpen, topbar: uiStore.topbarOpen };
}

beforeEach(() => {
	vp.mobile = false;
	uiStore.openSidebar();
	uiStore.openTopbar();
});

describe('uiStore.toggleChrome', () => {
	it('both open: closes both, then opens both', () => {
		uiStore.toggleChrome();
		expect(bars()).toEqual({ sidebar: false, topbar: false });
		uiStore.toggleChrome();
		expect(bars()).toEqual({ sidebar: true, topbar: true });
	});

	it('the desync the audit measured: workspace bar hidden, then Mod+\\ closes both', () => {
		uiStore.closeTopbar();
		uiStore.toggleChrome();
		// Before: the sidebar closed and the workspace bar REOPENED.
		expect(bars()).toEqual({ sidebar: false, topbar: false });
		uiStore.toggleChrome();
		expect(bars()).toEqual({ sidebar: true, topbar: true });
	});

	it('sidebar hidden alone: Mod+\\ closes the workspace bar too', () => {
		uiStore.closeSidebar();
		uiStore.toggleChrome();
		expect(bars()).toEqual({ sidebar: false, topbar: false });
	});

	it('the closed workspace bar is persisted, as its own button persists it', () => {
		uiStore.toggleChrome();
		expect(localStorage.getItem('pad-topbar')).toBe('closed');
		uiStore.toggleChrome();
		expect(localStorage.getItem('pad-topbar')).toBe('open');
	});

	it('on mobile only the sidebar moves: there is no workspace bar to show', () => {
		vp.mobile = true;
		uiStore.toggleChrome();
		expect(bars()).toEqual({ sidebar: false, topbar: true });
		uiStore.toggleChrome();
		expect(bars()).toEqual({ sidebar: true, topbar: true });
	});
});

describe('uiStore shortcuts sheet', () => {
	it('opens, closes and toggles', () => {
		uiStore.closeShortcuts();
		uiStore.openShortcuts();
		expect(uiStore.shortcutsOpen).toBe(true);
		uiStore.toggleShortcuts();
		expect(uiStore.shortcutsOpen).toBe(false);
	});
});
