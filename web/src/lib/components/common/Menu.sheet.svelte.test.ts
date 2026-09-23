/**
 * BUG-3157 codex round 3: at the mobile breakpoint Menu renders a BottomSheet,
 * and only a consumer whose content IS a menu (sheetMenu) gets a role="menu"
 * wrapper. Several sheetOnMobile consumers put other content there (a delete
 * confirmation, for one), which must not be announced as a menu.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import Menu from './Menu.svelte';

vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: {
		get isMobile() {
			return true;
		},
	},
}));

afterEach(() => cleanup());

const body = createRawSnippet(() => ({ render: () => '<p class="sheet-content">Delete this file?</p>' }));

function renderSheet(sheetMenu: boolean) {
	return render(Menu, {
		props: { open: true, onclose: () => {}, sheetOnMobile: true, sheetTitle: 'Confirm', sheetMenu, children: body },
	});
}

describe('Menu in sheet mode', () => {
	it('does not call non-menu content a menu', () => {
		renderSheet(false);
		expect(document.body.querySelector('.sheet-content'), 'precondition: the sheet rendered').not.toBeNull();
		expect(document.body.querySelector('[role="menu"]')).toBeNull();
	});

	it('CONTROL: sheetMenu wraps the content in a labelled menu', () => {
		renderSheet(true);
		const menu = document.body.querySelector('[role="menu"]');
		expect(menu).not.toBeNull();
		expect(menu!.getAttribute('aria-label')).toBe('Confirm');
		expect(menu!.querySelector('.sheet-content')).not.toBeNull();
	});
});
