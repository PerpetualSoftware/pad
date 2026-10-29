import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }] },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => true },
}));
vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: { isMobile: true, dragDisabled: false },
}));
// Expose the actual zone options without simulating the library's pointer machinery.
vi.mock('$lib/collections/lockableDndzone', () => ({
	lockableDndzone: (node: HTMLElement, options: { dragDisabled?: boolean; dropFromOthersDisabled?: boolean }) => {
		const update = (next: typeof options) => {
			node.dataset.dragDisabled = String(next.dragDisabled);
			node.dataset.dropDisabled = String(next.dropFromOthersDisabled);
		};
		update(options);
		return { update };
	},
}));

import BoardView from './BoardView.svelte';
import ListView from './ListView.svelte';
import TableView from './TableView.svelte';

const collection = {
	id: 'c1', workspace_id: 'ws1', name: 'Cars', slug: 'cars', prefix: 'CAR',
	schema: JSON.stringify({ fields: [{ key: 'status', label: 'Status', type: 'select', options: ['constructor', 'open'] }] }),
	settings: '{}',
} as Collection;
const items = ['constructor', 'open'].flatMap((status) => ['older', 'newer'].map((age, i) => ({
	id: `${status}-${age}`, slug: `${status}-${age}`, title: `${status}-${age}`,
	workspace_id: 'ws1', collection_id: 'c1', fields: JSON.stringify({ status }), tags: '[]',
	sort_order: 0, created_at: `2026-0${i + 1}-01T00:00:00Z`, updated_at: `2026-0${i + 1}-01T00:00:00Z`,
}) as Item));
const domOrder = (zone: Element) => [...zone.querySelectorAll('.card-title')].map((el) => el.textContent?.trim());

afterEach(cleanup);

describe('lane reversal (BUG-3994)', () => {
	it('reverses only the selected constructor lane, reports its order, and Page default clears the override', async () => {
		const onColumnsRendered = vi.fn();
		const r = render(BoardView, { props: {
			items, collection, sortMode: 'created', onLaneChange: vi.fn(), onColumnsRendered,
		} });
		const zones = r.container.querySelectorAll('.column-cards');
		expect(domOrder(zones[0])).toEqual(['constructor-newer', 'constructor-older']);
		const openSort = async () => {
			await fireEvent.click(r.getByRole('button', { name: 'Constructor lane actions' }));
			await fireEvent.click(r.getByRole('menuitem', { name: /Sort lane by/ }));
		};
		await openSort();
		// Negative control on base: this item does not exist.
		await fireEvent.click(r.getByRole('menuitemcheckbox', { name: 'Reverse order' }));
		await tick();
		expect(domOrder(zones[0])).toEqual(['constructor-older', 'constructor-newer']);
		expect(domOrder(zones[1])).toEqual(['open-newer', 'open-older']);
		expect(onColumnsRendered.mock.lastCall?.[0].map((c: { items: Item[] }) => c.items.map((i) => i.id)))
			.toEqual([['constructor-older', 'constructor-newer'], ['open-newer', 'open-older']]);
		expect(r.getByRole('menuitemcheckbox', { name: 'Reverse order' })).toHaveAttribute('aria-checked', 'true');
		await fireEvent.click(r.getByRole('menuitemradio', { name: /Page default/ }));
		await tick();
		expect(domOrder(zones[0])).toEqual(['constructor-newer', 'constructor-older']);
	});

	it('a manual lane override disables only that lane and rejects an incoming drop', async () => {
		const onReorder = vi.fn();
		const onLaneChange = vi.fn();
		const r = render(BoardView, { props: { items, collection, onReorder, onLaneChange } });
		await fireEvent.click(r.getByRole('button', { name: 'Constructor lane actions' }));
		await fireEvent.click(r.getByRole('menuitem', { name: /Sort lane by/ }));
		await fireEvent.click(r.getByRole('menuitemcheckbox', { name: 'Reverse order' }));
		const zones = r.container.querySelectorAll('.column-cards');
		expect(zones[0]).toHaveAttribute('data-drag-disabled', 'true');
		expect(zones[0]).toHaveAttribute('data-drop-disabled', 'true');
		expect(zones[1]).toHaveAttribute('data-drag-disabled', 'false');
		await fireEvent(zones[0], new CustomEvent('finalize', { detail: {
			items: [items[2], items[1], items[0]], info: { trigger: 'droppedIntoZone', id: items[2].id },
		} }));
		expect(onReorder).not.toHaveBeenCalled();
		expect(onLaneChange).not.toHaveBeenCalled();
	});

	it('inherits the page reversal, can override it, and preserves search relevance', async () => {
		const props = { items, collection, sortMode: 'created' as const, sortReversed: true, onLaneChange: vi.fn() };
		const r = render(BoardView, { props });
		const zone = r.container.querySelector('.column-cards')!;
		expect(domOrder(zone)).toEqual(['constructor-older', 'constructor-newer']);
		await fireEvent.click(r.getByRole('button', { name: 'Constructor lane actions' }));
		await fireEvent.click(r.getByRole('menuitem', { name: /Sort lane by/ }));
		await fireEvent.click(r.getByRole('menuitemcheckbox', { name: 'Reverse order' }));
		expect(domOrder(zone)).toEqual(['constructor-newer', 'constructor-older']);
		await r.rerender({ ...props, preserveOrder: true });
		expect(domOrder(zone)).toEqual(['constructor-older', 'constructor-newer']);
	});

	for (const view of ['board', 'list', 'table'] as const) {
		for (const reversed of [false, true]) {
			it(`${view}: manual reversed=${reversed} gates drag and reorder actions`, async () => {
				const onReorder = vi.fn();
				const component = { board: BoardView, list: ListView, table: TableView }[view];
				const r = render(component as never, { props: {
					items, collection, sortMode: 'manual', sortReversed: reversed, canEdit: true,
					onReorder, onLaneChange: vi.fn(), onStatusChange: vi.fn(), groupField: 'status',
				} as never });
				await tick();
				const zones = r.container.querySelectorAll(view === 'board' ? '.column-cards' : '.group-items');
				if (view !== 'table') {
					expect(zones.length).toBe(2);
					for (const zone of zones) {
						expect(zone).toHaveAttribute('data-drag-disabled', String(reversed));
						if (reversed) expect(zone).toHaveAttribute('data-drop-disabled', 'true');
					}
				}
				if (view === 'table') {
					expect(r.queryAllByRole('button', { name: /Reorder / }).length > 0).toBe(!reversed);
				} else {
					await fireEvent.click(r.getAllByRole('button', { name: 'Actions for constructor-newer' })[0]);
					const menu = within(r.getByRole('menu', { name: 'Actions for constructor-newer' }));
					expect(menu.queryByRole('menuitem', { name: /Move to top/ }) !== null).toBe(!reversed);
				}
				if (reversed) {
					// A late finalize must not persist the reversed rendered indexes either.
					for (const zone of zones) {
						await fireEvent(zone, new CustomEvent('finalize', { detail: {
							items: [items[1], items[0]], info: { trigger: 'droppedIntoZone', id: items[1].id },
						} }));
					}
					expect(onReorder).not.toHaveBeenCalled();
				}
			});
		}
	}
});
