// TASK-2248 U1: the share PAGE runs its markdown through the inert pass, on
// both paths that render a body (the direct item share and the collection
// inline-expand read the same `sanitizeMarkdown`). `shareRender.svelte.test.ts`
// covers the pass itself; this covers the binding (team CONVE-19).
import { describe, it, expect, vi, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';

const share = vi.hoisted(() => ({ payload: null as unknown }));

vi.mock('$lib/api/client', () => ({
	api: { share: { get: vi.fn(async () => share.payload) } },
	withRequestDeadline: vi.fn(),
}));

import { page } from '$app/state';
import SharePage from './+page.svelte';

async function settle() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

afterEach(() => cleanup());

describe('share page: the item body is made inert for an anonymous viewer (TASK-2248)', () => {
	it('wiki-links read as text, an internal link loses its href, and an external one keeps it', async () => {
		page.params = { token: 'tok' };
		share.payload = {
			type: 'item',
			item: {
				title: 'Navbar',
				ref: 'TASK-5',
				fields: {},
				content: 'See [[TASK-5]], [[IDEA-9]] and [the plan](/dave/docapp/plans/PLAN-2); also [site](https://getpad.dev).',
			},
		};
		const { container } = render(SharePage);
		await settle();
		// Found by its TEXT, not by anything the pass adds, so an unwired page
		// fails on the assertions below rather than on "did not render".
		const body = [...container.querySelectorAll('p')].find((p) => p.textContent?.startsWith('See '));
		expect(body, 'the body did not render').not.toBeNull();
		expect(body!.textContent).not.toContain('[[');
		expect([...body!.querySelectorAll('em.share-wiki-ref')].map((e) => e.textContent)).toEqual(['Navbar', 'IDEA-9']);
		expect([...body!.querySelectorAll('a')].map((a) => a.getAttribute('href'))).toEqual(['https://getpad.dev']);
		expect(body!.querySelector('span.share-internal-ref')!.textContent).toBe('the plan');
	});
});
