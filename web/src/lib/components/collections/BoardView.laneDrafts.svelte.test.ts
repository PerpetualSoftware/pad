// BUG-3043 — a lane draft whose lane no longer exists.
//
// Lead ruling, RE-HOME: the draft is shown in the Uncategorized lane, marked
// with the lane it came from, and saved there; where Uncategorized cannot
// receive it, it is kept and a notice names the lost lane.
//
// Before the fix the draft's card simply was not rendered — its lane was not in
// `renderColumns` — while its text stayed in the page's map and blocked "Save
// all". The legs here render the board the way the page mounts it, with the
// placement computed by the real `draftTargets`, so a drift between the
// module's answer and what the board shows fails here.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import type { Collection, FieldDef, Item } from '$lib/types';
import { draftKey, draftTargets } from '$lib/collections/laneDrafts';

/** A draft typed under the board's grouping field, `stage`. */
const k = (lane: string) => draftKey('stage', lane);

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }] },
}));

import BoardView from './BoardView.svelte';

function collection(fields: FieldDef[]): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Cars',
		slug: 'cars',
		icon: '',
		description: '',
		schema: JSON.stringify({ fields }),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'CAR',
	} as unknown as Collection;
}

function item(id: string, fields: Record<string, unknown>): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify(fields),
		tags: '[]',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

// Every item has a live stage, so Uncategorized holds no ITEM: only a
// re-homed draft can make it appear, which is what the forcing term is for.
const ITEMS = [item('one', { stage: 'a' })];

function renderBoard(field: FieldDef, draftText: Record<string, string>, extra: Record<string, unknown> = {}) {
	const onCreateInColumn = vi.fn(async () => ({}));
	const onDiscardDraft = vi.fn();
	const placement = draftTargets(draftText, field, {});
	const view = render(BoardView, {
		props: {
			items: ITEMS,
			collection: collection([field]),
			wsSlug: 'ws',
			groupField: 'stage',
			canEdit: true,
			onLaneChange: () => {},
			onCreateInColumn,
			onDiscardDraft,
			draftText,
			draftOpen: {},
			draftPlacement: placement,
			...extra,
		} as never,
	});
	return { ...view, onCreateInColumn, onDiscardDraft };
}

function laneTitles(container: HTMLElement): string[] {
	return [...container.querySelectorAll('.column-name')].map((el) => el.textContent?.trim() ?? '');
}

afterEach(() => cleanup());

describe('BoardView re-homes a draft whose lane is gone (BUG-3043)', () => {
	const renamed: FieldDef = { key: 'stage', label: 'Stage', type: 'select', options: ['a', 'b'] } as FieldDef;

	it('CONTROL: with no orphaned draft, no item-less Uncategorized lane is shown', () => {
		const { container } = renderBoard(renamed, { [k('a')]: 'kept in its lane' });
		expect(laneTitles(container)).not.toContain('Uncategorized');
		expect(container.querySelector('.lane-draft-rehomed')).toBeNull();
	});

	it('PATH 1: a draft in a deleted option shows in Uncategorized, marked, with its text', () => {
		const { container } = renderBoard(renamed, { [k('gone')]: 'my typed title' });
		expect(laneTitles(container)[0]).toBe('Uncategorized');
		const card = container.querySelector('.uncategorized-column .lane-draft-rehomed');
		expect(card, 'the draft renders inside the Uncategorized lane').toBeTruthy();
		expect(card!.querySelector('.lane-draft-moved')?.textContent).toContain('Moved from Gone');
		expect((card!.querySelector('textarea') as HTMLTextAreaElement).value).toBe('my typed title');
	});

	it('saving a re-homed draft passes its ORIGINAL key, where the page resolves the target', async () => {
		const { container, onCreateInColumn } = renderBoard(renamed, { [k('gone')]: 'my typed title' });
		const add = container.querySelector('.lane-draft-rehomed .lane-draft-add') as HTMLButtonElement;
		await fireEvent.click(add);
		expect(onCreateInColumn).toHaveBeenCalledWith(k('gone'), 'my typed title', true);
	});

	it('PATH 2: a retype to multi_relation re-homes every draft into the one lane left', () => {
		const retyped = { key: 'stage', label: 'Stage', type: 'multi_relation', options: ['a', 'b'] } as FieldDef;
		const { container } = renderBoard(retyped, { [k('a')]: 'first', [k('b')]: 'second' });
		const cards = container.querySelectorAll('.uncategorized-column .lane-draft-rehomed textarea');
		expect([...cards].map((t) => (t as HTMLTextAreaElement).value)).toEqual(['first', 'second']);
	});

	it('BUG-3214: a draft typed under ANOTHER field is re-homed, not shown in a same-named lane', () => {
		// Typed in status's `a` lane; the board is now grouped by `stage`, which
		// also has an `a` option. That lane is not the draft's home.
		const statusField: FieldDef = { key: 'status', label: 'Status', type: 'select', options: ['a'] } as FieldDef;
		const { container } = renderBoard(
			renamed,
			{ [draftKey('status', 'a')]: 'typed under status' },
			{ collection: collection([renamed, statusField]) }
		);
		const liveLaneDraft = [...container.querySelectorAll('.kanban-column:not(.uncategorized-column) textarea')];
		expect(liveLaneDraft, 'not in stage’s own “a” lane').toHaveLength(0);
		const card = container.querySelector('.uncategorized-column .lane-draft-rehomed');
		expect((card?.querySelector('textarea') as HTMLTextAreaElement).value).toBe('typed under status');
		expect(card?.querySelector('.lane-draft-moved')?.textContent).toContain('Moved from A (Status)');
	});

	it('EDGE 1: a draft Uncategorized cannot receive is NOT re-homed; the notice names its lane', async () => {
		const required = { key: 'stage', label: 'Stage', type: 'number', options: ['1'], required: true } as FieldDef;
		const draftText = { [k('7')]: 'unplaceable' };
		const { container, onDiscardDraft } = renderBoard(required, draftText, {
			blockedDraftNotices: [{ lane: k('7'), message: 'the “7” lane no longer exists' }],
		});
		expect(container.querySelector('.lane-draft-rehomed'), 'not moved into Uncategorized').toBeNull();
		const notice = container.querySelector('.board-draft-blocked');
		expect(notice?.textContent).toContain('the “7” lane no longer exists');
		expect(notice?.textContent).toContain('unplaceable');
		await fireEvent.click(notice!.querySelector('button')!);
		expect(onDiscardDraft).toHaveBeenCalledWith(k('7'));
	});
});
