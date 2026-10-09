// TASK-2226: PaneHost loads ItemDetail on demand, so a collection list no
// longer downloads and parses the editor stack before its first paint. The
// pane shows a status while the import is pending, the item view once it
// lands, and a retry when the import fails (offline, or a chunk a deploy
// replaced). The loader is mocked so each state is held open on purpose.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import Stub from './__stubs__/ItemDetailStub.svelte';

const loader = vi.hoisted(() => ({
	calls: 0,
	settle: [] as Array<{ resolve: (c: unknown) => void; reject: (e: unknown) => void }>
}));

vi.mock('$lib/components/items/itemDetailLoader', () => ({
	loadItemDetailComponent: () => {
		loader.calls++;
		return new Promise((resolve, reject) => loader.settle.push({ resolve, reject }));
	},
	prefetchItemDetail: () => {}
}));

import PaneHost from './PaneHost.svelte';

const props = {
	openItemRef: 'TASK-1',
	username: 'u',
	wsSlug: 'ws',
	collSlug: 'tasks',
	paneMintForRoute: 'TASK-1',
	onClose: vi.fn(),
	onGone: vi.fn(),
	onNavigateAway: vi.fn(),
	onOpenTarget: vi.fn(),
	onBack: vi.fn()
};

describe('PaneHost: ItemDetail on demand (TASK-2226)', () => {
	beforeEach(() => {
		loader.calls = 0;
		loader.settle.length = 0;
	});
	afterEach(() => cleanup());

	it('says it is loading, then renders the item view with the pane ref', async () => {
		render(PaneHost, { props });
		await tick();
		expect(loader.calls).toBe(1);
		expect(screen.getByRole('status').textContent).toContain('Loading');
		expect(screen.queryByTestId('item-detail-stub')).toBeNull();

		loader.settle[0].resolve(Stub);
		await vi.waitFor(() => expect(screen.getByTestId('item-detail-stub').textContent).toBe('TASK-1'));
		expect(screen.queryByRole('status')).toBeNull();
	});

	it('a failed import offers a retry, and the retry loads it', async () => {
		render(PaneHost, { props });
		await tick();
		loader.settle[0].reject(new Error('chunk gone'));
		const retry = await screen.findByRole('button', { name: 'Try again' });
		expect(screen.getByRole('alert').textContent).toContain("Couldn't load the item view");

		retry.click();
		await tick();
		expect(loader.calls).toBe(2);
		loader.settle[1].resolve(Stub);
		await vi.waitFor(() => expect(screen.getByTestId('item-detail-stub')).toBeTruthy());
	});
});
