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

	it('a reversed manual lane refuses drags out but takes a drop in as a status move, with no reindex', async () => {
		const onReorder = vi.fn();
		const onLaneChange = vi.fn();
		const r = render(BoardView, { props: { items, collection, onReorder, onLaneChange } });
		await fireEvent.click(r.getByRole('button', { name: 'Constructor lane actions' }));
		await fireEvent.click(r.getByRole('menuitem', { name: /Sort lane by/ }));
		await fireEvent.click(r.getByRole('menuitemcheckbox', { name: 'Reverse order' }));
		const zones = r.container.querySelectorAll('.column-cards');
		expect(zones[0]).toHaveAttribute('data-drag-disabled', 'true');
		expect(zones[0]).not.toHaveAttribute('data-drop-disabled', 'true');
		expect(zones[1]).toHaveAttribute('data-drag-disabled', 'false');
		// open-older dropped into the reversed constructor lane.
		await fireEvent(zones[0], new CustomEvent('finalize', { detail: {
			items: [items[2], items[1], items[0]], info: { trigger: 'droppedIntoZone', id: items[2].id },
		} }));
		expect(onLaneChange).toHaveBeenCalledTimes(1);
		expect(onLaneChange.mock.calls[0][0].id).toBe(items[2].id);
		expect(onLaneChange.mock.calls[0][1]).toBe('constructor');
		// Rendered indexes in a reversed lane are the stored order upside down;
		// writing them would flip it for everyone.
		expect(onReorder).not.toHaveBeenCalled();
		// The source lane (not reversed) still re-densifies what it has left.
		await fireEvent(zones[1], new CustomEvent('finalize', { detail: {
			items: [items[3]], info: { trigger: 'droppedIntoAnother', id: items[2].id },
		} }));
		expect(onReorder).toHaveBeenCalledTimes(1);
		expect(onReorder.mock.calls[0][0]).toEqual([{ slug: items[3].id, sort_order: 0 }]);
	});

	it('reversing a lane keeps it following the page sort mode', async () => {
		const props = { items, collection, sortMode: 'created' as const, onLaneChange: vi.fn() };
		const r = render(BoardView, { props });
		const zone = r.container.querySelector('.column-cards')!;
		await fireEvent.click(r.getByRole('button', { name: 'Constructor lane actions' }));
		await fireEvent.click(r.getByRole('menuitem', { name: /Sort lane by/ }));
		await fireEvent.click(r.getByRole('menuitemcheckbox', { name: 'Reverse order' }));
		expect(r.getByRole('menuitemradio', { name: /Page default/ })).toHaveAttribute('aria-checked', 'true');
		// created, reversed: oldest first.
		expect(domOrder(zone)).toEqual(['constructor-older', 'constructor-newer']);
		// The page switches to manual (oldest first); the lane's reversal now
		// applies to manual, so newest first. A pinned mode would stay put.
		await r.rerender({ ...props, sortMode: 'manual' });
		expect(domOrder(zone)).toEqual(['constructor-newer', 'constructor-older']);
	});

	it('list: a late finalize while reversed still clears the drag, so the list follows new props', async () => {
		const props = { items, collection, sortMode: 'manual' as const, sortReversed: true, canEdit: true, onReorder: vi.fn(), onLaneChange: vi.fn(), groupField: 'status' };
		const r = render(ListView, { props });
		await tick();
		const zone = r.container.querySelector('.group-items')!;
		await fireEvent(zone, new CustomEvent('consider', { detail: {
			items: [items[0], items[1]], info: { trigger: 'dragStarted', id: items[1].id },
		} }));
		await fireEvent(zone, new CustomEvent('finalize', { detail: {
			items: [items[0], items[1]], info: { trigger: 'droppedIntoZone', id: items[1].id },
		} }));
		expect(props.onReorder).not.toHaveBeenCalled();
		const added = { ...items[0], id: 'constructor-added', slug: 'constructor-added', title: 'constructor-added', created_at: '2026-03-01T00:00:00Z' } as Item;
		await r.rerender({ ...props, items: [...items, added] });
		await tick();
		expect(domOrder(zone)).toEqual(['constructor-added', 'constructor-newer', 'constructor-older']);
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
