// BUG-3799 (the director's audit of #2145): the markdown sanitizer keeps any
// class, so a share body could reuse the app's global CSS to overlay the page,
// e.g. attachment-crop-modal-shell (a 95vw x 92vh panel) for a fake sign-in,
// or sr-only to hide the genuine text. On the share path, classes are now
// restricted to what the renderer itself emits. Every body path is checked:
// the item share's marked pipeline, its fallback, and the collection expand.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { readFileSync } from 'node:fs';

const share = vi.hoisted(() => ({ payload: null as unknown, throwRender: false }));

vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {}
	return {
		PadApiError,
		api: { share: { get: vi.fn(async () => share.payload) } },
		withRequestDeadline: vi.fn(),
	};
});

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

const BODY = [
	'<div class="attachment-crop-modal-shell"><p>Sign in to Pad to keep reading.</p></div>',
	'',
	'<p class="sr-only">Genuine text an editor wanted hidden.</p>',
	'',
	'<span class="external-link file-chip">Wrong-tag classes.</span>',
	'',
	'<p>Password: <input type="password" name="pw"></p>',
	'',
	'<p role="dialog" data-print-hide>Attribute-selector probe.</p>',
	'',
	'- [x] a task',
	'',
	'Body marker with [a link](https://getpad.dev).',
	'',
	'```js',
	'const a = 1;',
	'```',
].join('\n');

async function settle() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

function expectRestricted(scope: Element, opts: { marked: boolean }) {
	expect(scope.textContent, 'the body did not render').toContain('Body marker');
	expect(scope.querySelector('.attachment-crop-modal-shell'), 'an app overlay class reached the page').toBeNull();
	expect(scope.querySelector('.sr-only'), 'an app hiding class reached the page').toBeNull();
	const wrongTag = [...scope.querySelectorAll('span')].find((s) => s.textContent?.includes('Wrong-tag'));
	expect(wrongTag, 'premise: the wrong-tag span rendered').toBeTruthy();
	expect(wrongTag!.getAttribute('class'), 'a class kept on a tag the renderer never puts it on').toBeNull();
	// app.css also selects [role="dialog"] and [data-print-hide]; the
	// sanitizer allowlist drops both, and this pins it on the share path.
	expect(scope.querySelector('[role], [data-print-hide]'), 'an attribute app CSS selects reached the page').toBeNull();
	// No text field to type into, which the global `input` rule would style.
	expect([...scope.querySelectorAll('input')].filter((i) => i.getAttribute('type') !== 'checkbox'), 'a form field reached the page').toEqual([]);
	// The text survives: only the class is stripped.
	expect(scope.textContent).toContain('Sign in to Pad');
	if (opts.marked) {
		// What the renderer itself emits is kept.
		expect(scope.querySelector('code.language-js'), "the renderer's language class was stripped").not.toBeNull();
		expect(scope.querySelector('a.external-link'), "the renderer's external-link class was stripped").not.toBeNull();
		const box = scope.querySelector('input[type="checkbox"]');
		expect(box, "the renderer's task-list checkbox was stripped").not.toBeNull();
		expect(box!.hasAttribute('disabled')).toBe(true);
	}
}

async function renderItemShare() {
	page.params = { token: 'tok' };
	share.payload = { type: 'item', item: { title: 'Notes', ref: 'DOC-1', fields: {}, content: BODY } };
	const { container } = render(SharePage);
	await settle();
	return container;
}

afterEach(() => {
	cleanup();
	share.throwRender = false;
});

describe('share page: a body cannot borrow the app CSS (BUG-3799)', () => {
	it('premise: the probe classes are styled by the global stylesheet the share page loads', () => {
		// The root layout, which the share route sits under, imports app.css.
		// Paths are from web/, the directory vitest runs in.
		const layout = readFileSync('src/routes/+layout.svelte', 'utf8');
		expect(layout).toMatch(/import\s+['"][^'"]*app\.css['"]/);
		const appCss = readFileSync('src/app.css', 'utf8');
		expect(appCss).toMatch(/\.attachment-crop-modal-shell\s*\{[^}]*width:\s*min\(95vw/);
		expect(appCss).toMatch(/\.sr-only\s*\{[^}]*position:\s*absolute/);
	});

	it('the item share: marked pipeline', async () => {
		expectRestricted(await renderItemShare(), { marked: true });
	});

	it('the item share: fallback when rendering throws', async () => {
		share.throwRender = true;
		expectRestricted(await renderItemShare(), { marked: false });
	});

	it('the collection share: inline expand', async () => {
		page.params = { token: 'tok' };
		(page as unknown as { url: URL }).url = new URL(window.location.href);
		share.payload = { type: 'collection', collection: { name: 'Shared' }, items: [{ ref: 'DOC-1', title: 'Doc', content: BODY }] };
		const { container } = render(SharePage);
		await settle();
		const opener = container.querySelector('[aria-expanded]');
		expect(opener, 'the collection did not render an expandable item').not.toBeNull();
		await fireEvent.click(opener!);
		await settle();
		const panel = container.querySelector('.item-expansion');
		expect(panel, 'the item did not expand').not.toBeNull();
		expectRestricted(panel!, { marked: true });
	});
});
