// BUG-3492: j/k in list and table views step the rows ON SCREEN. The views
// report the order they render (`onOrderRendered`) and the page steps it with
// listKeyNav, the way the board steps BoardView's columns. The page used to
// step `filteredItems` (updated order) while the list rendered its sort mode's
// order, so after an edit to an older item, j went to a row that was not the
// one below.
//
// Pinned here for every sort mode, both views, and collapsed groups: the
// reported order IS the DOM order, and a j step from the row above the edited
// older item lands on the row the user sees below it.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, Item } from '$lib/types';
import type { SortMode } from '$lib/collections/itemSort';
import { listKeyNav } from '$lib/collections/listNav';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }] },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => true },
}));

import ListView from './ListView.svelte';
import TableView from './TableView.svelte';

const FIELDS = [
	{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
	{ key: 'priority', label: 'Priority', type: 'select', options: ['high', 'low'] },
];

function collection(): Collection {
	return {
		id: 'c1', workspace_id: 'ws1', name: 'Cars', slug: 'cars', icon: '', description: '',
		schema: JSON.stringify({ fields: FIELDS }), settings: JSON.stringify({}),
		sort_order: 0, is_default: true, is_system: false,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', prefix: 'CAR',
	} as unknown as Collection;
}

function item(id: string, created: string, updated: string, fields: Record<string, unknown> = {}): Item {
	return {
		id, workspace_id: 'ws1', collection_id: 'c1', slug: id, title: id, content: '',
		fields: JSON.stringify({ status: 'open', ...fields }), tags: '[]', status: 'open',
		sort_order: 0, item_number: 1, created_at: created, updated_at: updated,
	} as unknown as Item;
}

// The shape from the trace: rows created in order, and one OLD row edited just
// now. In updated order (the page's `filteredItems`) `old-edited` sits right
// after `newest`; on screen in manual/created order it is last.
const ITEMS: Item[] = [
	item('old-edited', '2026-01-01T00:00:00Z', '2026-03-01T00:00:04Z', { priority: 'low' }),
	item('newest', '2026-03-01T00:00:05Z', '2026-03-01T00:00:05Z', { priority: 'high' }),
	item('middle', '2026-03-01T00:00:03Z', '2026-03-01T00:00:03Z', { priority: 'low' }),
	item('oldest', '2026-03-01T00:00:01Z', '2026-03-01T00:00:01Z', { priority: 'high', status: 'done' }),
];
// What the page holds: updated DESC.
const FILTERED = [...ITEMS].sort((a, b) => (a.updated_at < b.updated_at ? 1 : -1));

const MODES: SortMode[] = ['manual', 'priority', 'updated', 'created', 'title'];

afterEach(() => cleanup());

const listDom = (c: HTMLElement) =>
	[...c.querySelectorAll('.item-group')].flatMap((g) =>
		[...g.querySelectorAll('.card-title')].map((el) => el.textContent?.trim() ?? ''),
	);
const tableDom = (c: HTMLElement) =>
	[...c.querySelectorAll('.table-row:not(.table-header) .title-link')].map((el) => el.textContent?.trim() ?? '');

function mount(view: 'list' | 'table', sortMode: SortMode) {
	let reported: string[] = [];
	const onOrderRendered = (ids: string[]) => {
		reported = ids;
	};
	const props = {
		items: FILTERED, collection: collection(), wsSlug: 'ws', sortMode, canEdit: true,
		onOrderRendered, groupField: 'status', statusOptions: ['open', 'done'],
	};
	const r = render((view === 'list' ? ListView : TableView) as never, { props: props as never });
	return { r, reported: () => reported };
}

describe('the views report the order they render (BUG-3492)', () => {
	for (const view of ['list', 'table'] as const) {
		for (const mode of MODES) {
			it(`${view}, sort ${mode}: the reported order is the DOM order`, async () => {
				const { r, reported } = mount(view, mode);
				await tick();
				const dom = view === 'list' ? listDom(r.container) : tableDom(r.container);
				expect(dom.length, 'precondition: rows rendered').toBe(ITEMS.length);
				expect(reported()).toEqual(dom);
			});

			it(`${view}, sort ${mode}: j from each row lands on the row below it on screen`, async () => {
				const { r, reported } = mount(view, mode);
				await tick();
				const dom = view === 'list' ? listDom(r.container) : tableDom(r.container);
				for (let i = 0; i < dom.length - 1; i++) {
					expect(listKeyNav(reported(), dom[i], 1), `j from ${dom[i]}`).toBe(dom[i + 1]);
					expect(listKeyNav(reported(), dom[i + 1], -1), `k from ${dom[i + 1]}`).toBe(dom[i]);
				}
			});
		}
	}

	it('the regression itself: in manual order, j from the row above the edited older item is not that item', async () => {
		const { r, reported } = mount('list', 'manual');
		await tick();
		const dom = listDom(r.container);
		// The old, recently edited row is NOT below `newest` on screen...
		expect(dom.indexOf('old-edited')).not.toBe(dom.indexOf('newest') + 1);
		// ...but it IS in the page's updated order, which is what j used to step.
		expect(FILTERED[FILTERED.findIndex((i) => i.id === 'newest') + 1].id).toBe('old-edited');
		expect(listKeyNav(reported(), 'newest', 1)).toBe(dom[dom.indexOf('newest') + 1]);
	});

	it('a collapsed group drops out of the reported order, so j skips its rows', async () => {
		const { r, reported } = mount('list', 'manual');
		await tick();
		expect(reported()).toContain('oldest'); // status 'done', its own group
		const header = [...r.container.querySelectorAll('.group-header')].find((h) =>
			(h.textContent ?? '').toLowerCase().includes('done'),
		) as HTMLElement;
		expect(header, 'precondition: a done group header').toBeTruthy();
		await fireEvent.click(header);
		await tick();
		expect(reported()).not.toContain('oldest');
		expect(reported()).toEqual(listDom(r.container));
	});
	it('every group collapsed: the reported order is EMPTY (not absent), so j has nowhere to go', async () => {
		const { r, reported } = mount('list', 'manual');
		await tick();
		for (const h of [...r.container.querySelectorAll('.group-header')] as HTMLElement[]) {
			await fireEvent.click(h);
		}
		await tick();
		expect(listDom(r.container)).toEqual([]);
		expect(reported()).toEqual([]);
		expect(listKeyNav(reported(), null, 1)).toBeNull();
	});

	it('table: a column-header sort is reported in the order it renders', async () => {
		const { r, reported } = mount('table', 'manual');
		await tick();
		const before = tableDom(r.container);
		const header = r.container.querySelector('.sort-btn[title="Priority"]') as HTMLElement;
		expect(header, 'precondition: a sortable Priority header').toBeTruthy();
		await fireEvent.click(header);
		await tick();
		const dom = tableDom(r.container);
		const prio = (t: string) => JSON.parse(ITEMS.find((i) => i.title === t)!.fields as string).priority;
		expect(dom, 'precondition: the header sort changed the order').not.toEqual(before);
		expect(dom.map(prio), 'precondition: rows are in priority order').toEqual([...dom.map(prio)].sort());
		expect(reported()).toEqual(dom);
	});
});
