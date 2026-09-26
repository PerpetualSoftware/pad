// TASK-2248 U2: the direct item share draws its ```mermaid blocks (C120).
// `shareMermaid.svelte.test.ts` covers the pass and the collection
// inline-expand; this covers the page's own binding (team CONVE-19).
import { describe, it, expect, vi, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';

const share = vi.hoisted(() => ({ payload: null as unknown }));
vi.mock('$lib/api/client', () => ({
	api: { share: { get: vi.fn(async () => share.payload) } },
	withRequestDeadline: vi.fn(),
}));
vi.mock('mermaid', () => ({
	default: { initialize: vi.fn(), render: vi.fn(async () => ({ svg: '<svg class="drawn"></svg>' })) },
}));

import { page } from '$app/state';
import SharePage from './+page.svelte';

afterEach(() => cleanup());

describe('share page: a direct item share draws its mermaid blocks (TASK-2248 U2)', () => {
	it('the diagram replaces the raw source on screen', async () => {
		page.params = { token: 'tok' };
		share.payload = {
			type: 'item',
			item: { title: 'Flow', ref: 'DOC-1', fields: {}, content: '```mermaid\ngraph TD\nA-->B\n```' },
		};
		const { container } = render(SharePage);
		await vi.waitFor(() => expect(container.querySelector('.item-content .share-mermaid svg.drawn')).not.toBeNull());
		expect((container.querySelector('.item-content pre') as HTMLElement).hidden).toBe(true);
	});
});
