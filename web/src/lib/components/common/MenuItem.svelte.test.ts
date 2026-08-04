// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// MenuItem grew two capabilities for the attachment options panel
// (PLAN-2392 DR-5 / DR-3b, TASK-2422): an icon SNIPPET alongside the existing
// string icon, and an ANCHOR branch so a row can be a real `<a download>` or
// an Open-in-new-tab link. Both are additive, so the first assertion here is
// that the plain button row every existing call site uses is unchanged.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick } from 'svelte';
import MenuItem from './MenuItem.svelte';

const label = createRawSnippet(() => ({ render: () => `<span>Download</span>` }));
const svgIcon = createRawSnippet(() => ({
	render: () => `<svg data-testid="icon-svg" viewBox="0 0 24 24"><path d="M0 0h1" /></svg>`,
}));

function row(): HTMLElement {
	const el = document.querySelector('.mi');
	if (!el) throw new Error('.mi not found');
	return el as HTMLElement;
}

/** What Menu.svelte's keyboard navigation actually queries (Menu.svelte:130). */
function menuNavigable(): HTMLElement[] {
	return Array.from(
		document.querySelectorAll<HTMLElement>('[role^="menuitem"]:not(:disabled)')
	);
}

afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('MenuItem.svelte', () => {
	it('still renders the existing string-icon button row unchanged', async () => {
		const onclick = vi.fn();
		render(MenuItem, { props: { icon: '🗑', hint: '›', danger: true, onclick, children: label } });
		await tick();

		const el = row();
		expect(el.tagName).toBe('BUTTON');
		expect(el.getAttribute('type')).toBe('button');
		expect(el.getAttribute('role')).toBe('menuitem');
		expect(el.classList.contains('danger')).toBe(true);
		expect(el.querySelector('.mi-icon')?.textContent).toBe('🗑');
		expect(el.querySelector('.mi-hint')?.textContent).toBe('›');
		expect(el.hasAttribute('href')).toBe(false);

		(el as HTMLButtonElement).click();
		expect(onclick).toHaveBeenCalledTimes(1);
	});

	it('keeps the menuitemradio + check row for the checked variant', async () => {
		render(MenuItem, { props: { checked: true, describedBy: 'desc-1', children: label } });
		await tick();

		const el = row();
		expect(el.getAttribute('role')).toBe('menuitemradio');
		expect(el.getAttribute('aria-checked')).toBe('true');
		expect(el.getAttribute('aria-describedby')).toBe('desc-1');
		expect(el.querySelector('.mi-check')?.textContent).toBe('✓');
	});

	it('renders an icon snippet as markup, not as literal angle brackets', async () => {
		render(MenuItem, { props: { iconSnippet: svgIcon, children: label } });
		await tick();

		const icon = row().querySelector('.mi-icon');
		expect(icon?.getAttribute('aria-hidden')).toBe('true');
		// The whole point: an <svg> ELEMENT, not the text "<svg ...>".
		expect(icon?.querySelector('svg')).not.toBeNull();
		expect(icon?.textContent).not.toContain('<svg');
	});

	it('prefers the icon snippet when both icon forms are supplied', async () => {
		render(MenuItem, { props: { icon: '🗑', iconSnippet: svgIcon, children: label } });
		await tick();

		const icons = document.querySelectorAll('.mi-icon');
		expect(icons).toHaveLength(1);
		expect(icons[0].querySelector('svg')).not.toBeNull();
		expect(icons[0].textContent).not.toContain('🗑');
	});

	it('renders an anchor row with href/download and keeps menuitem semantics', async () => {
		render(MenuItem, {
			props: {
				href: '/api/v1/workspaces/ws/attachments/att-1',
				download: 'report.pdf',
				icon: '⇩',
				children: label,
			},
		});
		await tick();

		const el = row();
		expect(el.tagName).toBe('A');
		expect(el.getAttribute('href')).toBe('/api/v1/workspaces/ws/attachments/att-1');
		// A REAL download attribute — the server's inline disposition would
		// otherwise open the file rather than save it (DR-16).
		expect(el.getAttribute('download')).toBe('report.pdf');
		expect(el.getAttribute('role')).toBe('menuitem');
		expect(el.classList.contains('mi')).toBe(true);
		// Reachable by Menu's arrow-key navigation exactly like a button row.
		expect(menuNavigable()).toEqual([el]);
	});

	it('passes target/rel through for the open-in-new-tab anchor', async () => {
		render(MenuItem, {
			props: {
				href: '/api/v1/workspaces/ws/attachments/att-1',
				target: '_blank',
				rel: 'noopener noreferrer',
				children: label,
			},
		});
		await tick();

		const el = row();
		expect(el.tagName).toBe('A');
		expect(el.getAttribute('target')).toBe('_blank');
		expect(el.getAttribute('rel')).toBe('noopener noreferrer');
		expect(el.hasAttribute('download')).toBe(false);
	});

	it('activates an anchor row on Space, like every other row in the menu', async () => {
		// A native anchor activates on Enter but not Space, and role="menuitem"
		// does not add the behavior — so without an explicit handler, Space
		// would silently do nothing on Download and Open while working on
		// every button row beside them.
		const onclick = vi.fn();
		render(MenuItem, {
			props: { href: '/api/v1/workspaces/ws/attachments/att-1', onclick, children: label },
		});
		await tick();

		const el = row() as HTMLAnchorElement;
		const evt = new KeyboardEvent('keydown', { key: ' ', bubbles: true, cancelable: true });
		el.dispatchEvent(evt);
		await tick();

		expect(onclick).toHaveBeenCalledTimes(1);
		// Space scrolls the page by default; the open menu is the active surface.
		expect(evt.defaultPrevented).toBe(true);
	});

	it('renders a disabled anchor as a disabled button so keyboard nav skips it', async () => {
		render(MenuItem, {
			props: { href: '/api/v1/workspaces/ws/attachments/att-1', disabled: true, children: label },
		});
		await tick();

		const el = row();
		// `<a>` ignores `disabled`, stays focusable and still navigates — a
		// disabled link would be a live link that only LOOKS unavailable.
		expect(el.tagName).toBe('BUTTON');
		expect((el as HTMLButtonElement).disabled).toBe(true);
		expect(el.hasAttribute('href')).toBe(false);
		expect(menuNavigable()).toEqual([]);
	});
});
