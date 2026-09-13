// The ITEM-PANE picker for a `multi_relation` (PLAN-2857 U4, web population row W7).
//
// W7 is the row the rest of U4's web half exists to make usable: a type the
// schema editor can DECLARE but no item pane can EDIT is half-shipped. The
// implementation is a parameterisation rather than a widening — the four
// resolution functions and the chip snippet now take ONE raw reference, and the
// scalar path calls them with its single element — so the instrument comes in
// two halves:
//
//   * `FieldEditor.relation.svelte.test.ts` (40 tests) is the evidence the
//     parameterisation moved NOTHING on the scalar path. Those tests were not
//     touched by this unit.
//   * this file is the evidence the list behaviour is what U4 ruled: ordered,
//     duplicates refused, `[]` is the one spelling of none.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import { tick } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';

const {
	localIndexMock,
	localSearchMock,
	searchApi,
	createApi,
	collectionStoreMock,
	workspaceStoreMock,
	toastMock,
} = vi.hoisted(() => ({
	localIndexMock: {
		bootstrapStateFor: vi.fn(),
		findByIdOrSlug: vi.fn(),
		getByCollection: vi.fn(),
		cursorFor: vi.fn(),
		upsert: vi.fn(),
		scopeEpochFor: vi.fn(),
		pendingResyncFor: vi.fn(),
		resetGenerationFor: vi.fn(),
	},
	localSearchMock: { search: vi.fn(), epoch: vi.fn() },
	searchApi: vi.fn(),
	createApi: vi.fn(),
	collectionStoreMock: {
		collections: [] as { id: string; slug: string; name?: string }[],
		collectionsAreFreshFor: vi.fn(),
	},
	workspaceStoreMock: { canEditCollection: vi.fn() },
	toastMock: { show: vi.fn() },
}));
vi.mock('$lib/api/client', () => ({
	api: {
		search: (...a: unknown[]) => searchApi(...a),
		items: { create: (...a: unknown[]) => createApi(...a) },
	},
}));
vi.mock('$lib/stores/localIndex.svelte', () => ({ localIndex: localIndexMock }));
vi.mock('$lib/stores/localSearch.svelte', () => ({ localSearch: localSearchMock }));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: collectionStoreMock }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: workspaceStoreMock }));
vi.mock('$lib/stores/toast.svelte', () => ({ toastStore: toastMock }));

import FieldEditor from './FieldEditor.svelte';

const RED = { id: 'uuid-red', title: 'Red', item_number: 3, collection_prefix: 'COLO', collection_slug: 'colors', slug: 'red', deleted_at: null };
const BLUE = { ...RED, id: 'uuid-blue', title: 'Blue', item_number: 4, slug: 'blue' };
const GREEN = { ...RED, id: 'uuid-green', title: 'Green', item_number: 5, slug: 'green' };
const GONE = { ...RED, id: 'uuid-gone', title: 'Retired Puce', item_number: 6, slug: 'retired-puce', deleted_at: '2026-01-01T00:00:00Z' };

/** The field under test. `relation` appears only in the CONTROL legs. */
const multi = { key: 'colors', label: 'Colours', type: 'multi_relation' as const, collection: 'colors' };
const scalar = { key: 'color', label: 'Colour', type: 'relation' as const, collection: 'colors' };

const rows = new Map<string, unknown>();
const epochs = new SvelteMap<string, number>();

beforeEach(() => {
	rows.clear();
	for (const r of [RED, BLUE, GREEN, GONE]) rows.set(r.id, r);
	epochs.clear();
	localIndexMock.bootstrapStateFor.mockReset().mockReturnValue('ready');
	localIndexMock.findByIdOrSlug.mockReset().mockImplementation((_ws: string, id: string) => rows.get(id) ?? null);
	localIndexMock.getByCollection.mockReset().mockReturnValue([RED, BLUE, GREEN]);
	localIndexMock.cursorFor.mockReset().mockReturnValue('0');
	localIndexMock.pendingResyncFor.mockReset().mockReturnValue(false);
	localSearchMock.search.mockReset().mockReturnValue([]);
	localSearchMock.epoch.mockReset().mockImplementation((ws: string) => epochs.get(ws) ?? 0);
	searchApi.mockReset().mockResolvedValue({ results: [] });
	collectionStoreMock.collections = [
		{ id: 'coll-colors', slug: 'colors', name: 'Colors' },
		{ id: 'coll-tasks', slug: 'tasks', name: 'Tasks' },
	];
	collectionStoreMock.collectionsAreFreshFor.mockReset().mockReturnValue(true);
	workspaceStoreMock.canEditCollection.mockReset().mockReturnValue(true);
	createApi.mockReset();
	toastMock.show.mockReset();
	localIndexMock.upsert.mockReset();
	localIndexMock.scopeEpochFor.mockReset().mockReturnValue(7);
	localIndexMock.resetGenerationFor.mockReset().mockReturnValue(3);
});
afterEach(() => { cleanup(); document.body.innerHTML = ''; });

/** The invariant that spans every leg: a UUID must never reach the user. */
function expectNoBareUuid() {
	expect(document.body.textContent ?? '').not.toMatch(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/i);
	expect(document.body.textContent ?? '').not.toContain('uuid-');
}

/** Buttons by their visible label, which is how the user finds them. */
function buttons(label: RegExp): HTMLButtonElement[] {
	return [...document.querySelectorAll('button')].filter((b) => label.test(b.textContent ?? '')) as HTMLButtonElement[];
}

describe('multi_relation — read-only rendering', () => {
	it('renders one chip per element, in the STORED ORDER', async () => {
		// Order is the whole reason this is a list and not a set: the field is
		// ruled ORDERED, so a render that sorted or de-duplicated would be
		// showing the user something other than what the item holds.
		render(FieldEditor, {
			props: { field: multi, value: [GREEN.id, RED.id, BLUE.id], wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} },
		});
		await tick();
		const titles = [...document.querySelectorAll('.relation-title')].map((n) => n.textContent);
		expect(titles).toEqual(['Green', 'Red', 'Blue']);
		expectNoBareUuid();
	});

	it('each chip is a link to its own target', async () => {
		render(FieldEditor, {
			props: { field: multi, value: [RED.id, BLUE.id], wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} },
		});
		await tick();
		const hrefs = [...document.querySelectorAll('a.relation-chip')].map((a) => a.getAttribute('href'));
		expect(hrefs).toEqual(['/dave/ws/colors/COLO-3', '/dave/ws/colors/COLO-4']);
	});

	it('mixes states per element — a deleted target does not poison its neighbours', async () => {
		render(FieldEditor, {
			props: { field: multi, value: [RED.id, GONE.id, 'not-an-id'], wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} },
		});
		await tick();
		expect(document.querySelectorAll('.relation-chip.is-deleted')).toHaveLength(1);
		expect(document.querySelectorAll('.relation-chip.is-unresolved')).toHaveLength(1);
		expect(document.querySelectorAll('a.relation-chip')).toHaveLength(1);
		expectNoBareUuid();
	});

	it('an EMPTY list renders the em-dash, exactly as an empty scalar does', async () => {
		// A list of zero chips would otherwise render as literally nothing, and
		// "no value" and "this field does not exist" would look identical.
		render(FieldEditor, { props: { field: multi, value: [], wsSlug: 'ws', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.querySelector('.relation-empty')).not.toBeNull();
		expect(document.body.textContent).toContain('—');
	});

	it('an ABSENT value renders the em-dash too — absent and [] are the one none', async () => {
		render(FieldEditor, { props: { field: multi, value: undefined, wsSlug: 'ws', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.querySelector('.relation-empty')).not.toBeNull();
	});

	it('a NON-ARRAY value reads as empty rather than as one reference', async () => {
		// No write door accepts this shape (`ValidateFields` refuses anything
		// but an array), so it should be unreachable — but rendering a bare
		// string as though the field held one target would state the opposite
		// of the type's contract, and a crash would take the whole pane.
		render(FieldEditor, { props: { field: multi, value: RED.id, wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.querySelectorAll('.relation-chip')).toHaveLength(0);
		expect(document.querySelector('.relation-empty')).not.toBeNull();
		expectNoBareUuid();
	});

	it('blank and non-string elements are dropped, not rendered', async () => {
		render(FieldEditor, {
			props: { field: multi, value: [RED.id, '   ', 7, null, BLUE.id], wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} },
		});
		await tick();
		const titles = [...document.querySelectorAll('.relation-title')].map((n) => n.textContent);
		expect(titles).toEqual(['Red', 'Blue']);
		expect(document.body.textContent).not.toContain('7');
		// COUNT the rendered units, not only the named ones: a blank element
		// renders as the chip's own empty arm (an em-dash), which the titles
		// assertion above cannot see at all. Without this the "dropped" claim
		// is satisfied by a list that kept all five and merely failed to name
		// three of them — and that list is also what would be WRITTEN BACK by
		// the next add or remove, into a door that refuses blank elements.
		expect(document.querySelectorAll('.relation-chip')).toHaveLength(2);
		expect(document.querySelectorAll('.relation-empty')).toHaveLength(0);
	});

	it('CONTROL: the scalar type still renders its single chip from a string', async () => {
		render(FieldEditor, { props: { field: scalar, value: RED.id, wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} } });
		await tick();
		expect([...document.querySelectorAll('.relation-title')].map((n) => n.textContent)).toEqual(['Red']);
	});
});

describe('multi_relation — the editor: add, remove, order', () => {
	const editable = { field: multi, wsSlug: 'ws', username: 'dave' } as const;

	it('gives every element its own Remove — not the scalar Change/Clear pair', async () => {
		render(FieldEditor, { props: { ...editable, value: [RED.id, BLUE.id], onchange: () => {} } });
		await tick();
		expect(buttons(/^\s*Remove\s*$/)).toHaveLength(2);
		expect(buttons(/Change/)).toHaveLength(0);
		expect(buttons(/^\s*Clear\s*$/)).toHaveLength(0);
	});

	it('Remove drops THAT element and preserves the order of the rest', async () => {
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange } });
		await tick();
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[1]);
		expect(onchange).toHaveBeenCalledTimes(1);
		expect(onchange.mock.calls[0][0]).toEqual([GREEN.id, BLUE.id]);
	});

	it('removing the LAST element writes [] — the shape, not an empty string', async () => {
		// `[]` is a valid multi_relation shape meaning "no targets"; normalising
		// it to an absent key belongs to the write door, and `''` is the SCALAR's
		// clear spelling, which this type's validator refuses outright.
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editable, value: [RED.id], onchange } });
		await tick();
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange).toHaveBeenCalledWith([]);
		expect(onchange).not.toHaveBeenCalledWith('');
	});

	it('choosing APPENDS to the end rather than replacing the list', async () => {
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editable, value: [RED.id], onchange } });
		await tick();
		await fireEvent.click(buttons(/\+\s*Add/)[0]);
		await tick();
		const option = [...document.querySelectorAll('button')].find((b) => (b.textContent ?? '').includes('Blue'));
		expect(option, 'the picker offered no Blue row to choose').toBeTruthy();
		await fireEvent.click(option!);
		expect(onchange).toHaveBeenCalledTimes(1);
		expect(onchange.mock.calls[0][0]).toEqual([RED.id, BLUE.id]);
	});

	it('an EMPTY field opens the picker, with no Add button to press first', async () => {
		// Same rule the scalar branch already follows: nothing to show means the
		// picker shows instead, rather than a button that reveals a search box.
		//
		// The last assertion is what makes this leg discriminate. The first two
		// pass against a tree that has never heard of `multi_relation` — an empty
		// value fell into the SCALAR branch, which also opens a picker and also
		// has no Add button — so on their own they measure the scalar code. What
		// distinguishes the branches is the SHAPE the picker writes.
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editable, value: [], onchange } });
		await tick();
		expect(document.querySelector('input')).not.toBeNull();
		expect(buttons(/\+\s*Add/)).toHaveLength(0);
		const option = [...document.querySelectorAll('button')].find((b) => (b.textContent ?? '').includes('Red'));
		expect(option, 'the picker offered no row to choose').toBeTruthy();
		await fireEvent.click(option!);
		expect(onchange).toHaveBeenCalledWith([RED.id]);
	});

	it('a non-empty field shows Add, and pressing it opens the picker', async () => {
		render(FieldEditor, { props: { ...editable, value: [RED.id], onchange: () => {} } });
		await tick();
		expect(document.querySelector('input')).toBeNull();
		await fireEvent.click(buttons(/\+\s*Add/)[0]);
		await tick();
		expect(document.querySelector('input')).not.toBeNull();
	});

	it('a chip in the list still opens in the pane instead of navigating', async () => {
		const onOpenTarget = vi.fn();
		render(FieldEditor, { props: { ...editable, value: [RED.id, BLUE.id], onchange: () => {}, onOpenTarget } });
		await tick();
		const chips = [...document.querySelectorAll('a.relation-chip')] as HTMLAnchorElement[];
		await fireEvent.click(chips[1]);
		expect(onOpenTarget).toHaveBeenCalledTimes(1);
		// The SECOND chip's target, not the first — the handler takes the row it
		// was rendered for rather than reading one shared value.
		expect(onOpenTarget.mock.calls[0][0]).toMatchObject({ ref: 'COLO-4', slug: 'blue', collectionSlug: 'colors' });
	});
});

describe('multi_relation — duplicates are prevented, not de-duplicated', () => {
	const editable = { field: multi, wsSlug: 'ws', username: 'dave' } as const;

	it('the picker does not offer an item the field already references', async () => {
		// The server REFUSES a duplicate (`duplicate_referent`) rather than
		// storing one copy, so the client's job is to make the refusable click
		// unavailable — not to silently collapse it afterwards.
		render(FieldEditor, { props: { ...editable, value: [RED.id], onchange: () => {} } });
		await tick();
		await fireEvent.click(buttons(/\+\s*Add/)[0]);
		await tick();
		const offered = [...document.querySelectorAll('button')].map((b) => b.textContent ?? '').join(' | ');
		expect(offered).not.toMatch(/\bRed\b/);
		// CONTROL: the other rows from the same list ARE offered, so the
		// assertion above is about the duplicate and not about an empty picker.
		expect(offered).toMatch(/\bBlue\b/);
		expect(offered).toMatch(/\bGreen\b/);
	});

	it('a soft-deleted target already in the list is excluded too', async () => {
		// It is already a member; that it is dead does not make room for a
		// second copy of it.
		localIndexMock.getByCollection.mockReturnValue([RED, BLUE, GONE]);
		render(FieldEditor, { props: { ...editable, value: [GONE.id], onchange: () => {} } });
		await tick();
		await fireEvent.click(buttons(/\+\s*Add/)[0]);
		await tick();
		const offered = [...document.querySelectorAll('button')].map((b) => b.textContent ?? '').join(' | ');
		expect(offered).not.toMatch(/Retired Puce/);
		expect(offered).toMatch(/\bBlue\b/);
	});

	it('excludes an element the LOCAL INDEX cannot resolve, so the cold picker cannot re-offer it', async () => {
		// The gap the raw half of the exclusion set exists for. While the index
		// is cold the picker searches the SERVER, whose rows were never in the
		// index — so an element that resolves to nothing here contributes no
		// resolved id, and is precisely the element whose target the server can
		// still hand back. The stored element IS the id, so excluding the raw
		// string is what closes it.
		const GHOST = { ...RED, id: 'uuid-ghost', title: 'Ghost', item_number: 12, slug: 'ghost' };
		localIndexMock.bootstrapStateFor.mockReturnValue('loading');
		searchApi.mockResolvedValue({ results: [{ item: GHOST }, { item: BLUE }], limit: 20 });
		render(FieldEditor, { props: { ...editable, value: [GHOST.id], onchange: () => {} } });
		await tick();
		// PRECONDITION: the element really is unresolvable here, so this leg is
		// about the raw spelling and not about a resolved id doing the work.
		expect(document.querySelector('.relation-chip.is-unresolved')).not.toBeNull();
		await fireEvent.click(buttons(/\+\s*Add/)[0]);
		await tick();
		const input = document.querySelector('input') as HTMLInputElement;
		await fireEvent.input(input, { target: { value: 'blue' } });
		await vi.waitFor(() => {
			const offered = [...document.querySelectorAll('button')].map((b) => b.textContent ?? '').join(' | ');
			// CONTROL first: the server answered and its OTHER row is offered,
			// so the absence below is an exclusion rather than an empty picker.
			expect(offered).toMatch(/\bBlue\b/);
			expect(offered).not.toMatch(/\bGhost\b/);
		});
	});

	it('CONTROL: with an empty field the picker offers every row', async () => {
		render(FieldEditor, { props: { ...editable, value: [], onchange: () => {} } });
		await tick();
		const offered = [...document.querySelectorAll('button')].map((b) => b.textContent ?? '').join(' | ');
		expect(offered).toMatch(/\bRed\b/);
		expect(offered).toMatch(/\bBlue\b/);
		expect(offered).toMatch(/\bGreen\b/);
	});
});

describe('multi_relation — inline create (PLAN-2857 U8) appends like any other choice', () => {
	const editable = { field: multi, wsSlug: 'ws', username: 'dave' } as const;

	it('a created item lands at the END of the existing list, not over it', async () => {
		// The create path and the pick path are two ways of choosing, and they
		// reach the field through one function. If they did not, a create on a
		// multi_relation would quietly replace everything already in it.
		const NEW = { ...RED, id: 'uuid-new', title: 'Puce', item_number: 9, slug: 'puce' };
		createApi.mockResolvedValue(NEW);
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editable, value: [RED.id, BLUE.id], onchange } });
		await tick();
		await fireEvent.click(buttons(/\+\s*Add/)[0]);
		await tick();
		const input = document.querySelector('input') as HTMLInputElement;
		await fireEvent.input(input, { target: { value: 'Puce' } });
		await tick();
		const createRow = [...document.querySelectorAll('button')].find((b) => /create/i.test(b.textContent ?? ''));
		expect(createRow, 'the picker offered no create row').toBeTruthy();
		await fireEvent.click(createRow!);
		await vi.waitFor(() => expect(onchange).toHaveBeenCalled());
		expect(onchange.mock.calls[0][0]).toEqual([RED.id, BLUE.id, NEW.id]);
	});
});

describe('multi_relation — the gate is the same gate', () => {
	it('renders read-only with no wsSlug, exactly as a scalar relation does', async () => {
		render(FieldEditor, { props: { field: multi, value: [RED.id], username: 'dave', onchange: () => {} } });
		await tick();
		// PRECONDITION for the two absence assertions: the component mounted and
		// took the RELATION path. Without it "no picker, no buttons" is also what
		// a component that rendered nothing at all looks like. Resolution needs a
		// `wsSlug`, so the chip degrades to unresolved here — which is itself the
		// honest state for a field whose workspace the caller withheld.
		expect(document.querySelector('.relation-chip')).not.toBeNull();
		expect(document.querySelector('input')).toBeNull();
		expect(buttons(/Remove|\+\s*Add/)).toHaveLength(0);
	});

	it('renders read-only when the declared target collection no longer exists', async () => {
		collectionStoreMock.collections = [{ id: 'coll-tasks', slug: 'tasks', name: 'Tasks' }];
		render(FieldEditor, { props: { field: multi, value: [RED.id], wsSlug: 'ws', username: 'dave', onchange: () => {} } });
		await tick();
		// Same precondition, and here the chip must still NAME the item: a
		// renamed target collection is not data loss, and the scalar branch has
		// carried that rule since TASK-2868.
		expect(document.body.textContent).toContain('Red');
		expect(document.querySelector('input')).toBeNull();
		expect(buttons(/Remove|\+\s*Add/)).toHaveLength(0);
	});
});
