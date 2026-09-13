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

describe('multi_relation — two edits before the first save lands', () => {
	const editable = { field: multi, wsSlug: 'ws', username: 'dave' } as const;

	it('bases the second edit on what was SENT, not on the stale prop', async () => {
		// codex round 7, P1, in my own W7 code. `value` only catches up after the
		// server round trip, so two quick removes both derived from it: remove
		// Grace then Red from [Grace,Red,Blue] sent [Red,Blue] and then
		// [Grace,Blue] — and the parent's 409 refetch-and-retry can persist the
		// second, putting Grace back. Every list edit is a WHOLE-LIST write, so a
		// stale base is a lost update rather than a harmless recompute.
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange } });
		await tick();

		const removes = buttons(/^\s*Remove\s*$/);
		expect(removes).toHaveLength(3);
		await fireEvent.click(removes[0]);
		await tick();
		// The prop has NOT been updated — that is the whole point: the parent is
		// still waiting on the server.
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);

		expect(onchange).toHaveBeenCalledTimes(2);
		expect(onchange.mock.calls[0][0]).toEqual([RED.id, BLUE.id]);
		expect(onchange.mock.calls[1][0]).toEqual([BLUE.id]);
	});

	it('CONTROL: a single edit still reads the prop', async () => {
		// Without this, holding a stale list forever would satisfy the leg above
		// while ignoring every value the server ever returns.
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editable, value: [GREEN.id, RED.id], onchange } });
		await tick();
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[0][0]).toEqual([RED.id]);
	});

	it('lets a change from elsewhere take over once the prop agrees', async () => {
		// The hold is released by CONTENT agreement, so an SSE update or the
		// parent's retry is authoritative the moment it lands. A hold that
		// outlived its own write would make this component the owner of the
		// value, which it is not.
		const onchange = vi.fn();
		const { rerender } = render(FieldEditor, {
			props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange },
		});
		await tick();
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[0][0]).toEqual([RED.id, BLUE.id]);

		// The server confirms that write, and then something else ADDS a row —
		// an SSE update, another tab, the parent's 409 refetch-and-retry.
		await rerender({ ...editable, value: [RED.id, BLUE.id], onchange });
		await tick();
		await rerender({ ...editable, value: [RED.id, BLUE.id, GREEN.id], onchange });
		await tick();

		// The new row has to be VISIBLE, or the hold is still in force and this
		// leg is measuring the held list rather than the prop. My first version
		// changed the value to a list of the same LENGTH, so a held list and the
		// new one produced the identical answer and the never-release mutant
		// survived.
		expect([...document.querySelectorAll('.relation-title')].map((n) => n.textContent))
			.toEqual(['Red', 'Blue', 'Green']);

		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[1][0]).toEqual([BLUE.id, GREEN.id]);
	});

	it('drops the held list when the component is reused for a different item', async () => {
		// `ItemDetail` keys its fields section on the item SLUG ONLY, so
		// switching workspaces to an item carrying the same ref reuses this
		// instance and never destroys it — the RETARGETED hazard the inline
		// create already fences. A held list surviving that would become the
		// base for an edit on a different row.
		const onchange = vi.fn();
		const { rerender } = render(FieldEditor, {
			props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange },
		});
		await tick();
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[0][0]).toEqual([RED.id, BLUE.id]);

		// Same instance, different workspace, an item whose value is its own.
		await rerender({ field: multi, wsSlug: 'other-ws', username: 'dave', value: [BLUE.id, RED.id], onchange });
		await tick();
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[1][0]).toEqual([RED.id]);
	});
});

describe('multi_relation — the hold survives an ordinary re-render', () => {
	const editable = { field: multi, wsSlug: 'ws', username: 'dave' } as const;

	it('keeps the sent list as the base when the parent re-renders mid-write', async () => {
		// THE LEG THAT SAYS THE FIX WORKS OUTSIDE A TEST. The parent re-renders
		// for all sorts of reasons while a write is in flight, and if that
		// released the hold the stale prop would come back as the base — the
		// original defect, reachable by a different route, and invisible to a
		// test whose two clicks happen with nothing in between.
		const onchange = vi.fn();
		const { rerender } = render(FieldEditor, {
			props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange },
		});
		await tick();
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[0][0]).toEqual([RED.id, BLUE.id]);

		// Same props, same stale value — the server has not answered yet.
		await rerender({ ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange });
		await tick();

		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[1][0]).toEqual([BLUE.id]);
	});
});

describe('multi_relation — the hold ends when the write does, not only when it succeeds', () => {
	const editable = { field: multi, wsSlug: 'ws', username: 'dave' } as const;

	/** A consumer that answers, eventually — the shape `ItemDetail` really has. */
	function gatedConsumer(outcome: 'resolve' | 'reject') {
		const gates: Array<() => void> = [];
		const onchange = vi.fn(
			() =>
				new Promise<void>((resolve, reject) => {
					gates.push(() => (outcome === 'resolve' ? resolve() : reject(new Error('refused'))));
				})
		);
		return { onchange, gates };
	}

	/** Let the settlement continuation and its flush run. */
	async function settle() {
		await tick();
		await tick();
		await tick();
	}

	const titles = () => [...document.querySelectorAll('.relation-title')].map((n) => n.textContent);

	it('a REFUSED write stops holding, so the field shows server truth again', async () => {
		// codex round 8, R8-2. The hold released only on prop AGREEMENT, and a
		// write the server refuses never agrees — so a rejected clear of a
		// required field kept showing the rejected value forever and ignored
		// every later server value. Settlement is the question the hold is for:
		// the write is over either way.
		const { onchange, gates } = gatedConsumer('reject');
		render(FieldEditor, { props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange } });
		await tick();

		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[0][0]).toEqual([RED.id, BLUE.id]);
		// Optimistic while the write is outstanding — that part is unchanged.
		expect(titles()).toEqual(['Red', 'Blue']);

		gates[0]();
		await settle();

		// The parent refused and left the value alone, so the prop is still the
		// original list. Seeing it again is the fix; still seeing ['Red','Blue']
		// is the defect.
		expect(titles()).toEqual(['Green', 'Red', 'Blue']);
		expectNoBareUuid();
	});

	it('and the NEXT edit bases on the prop again, not on the abandoned list', async () => {
		// The release has to reach the write path, not just the render: a hold
		// left in place would make the refused list the base for whatever the
		// user does next.
		const { onchange, gates } = gatedConsumer('reject');
		render(FieldEditor, { props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange } });
		await tick();

		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		gates[0]();
		await settle();

		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[1][0]).toEqual([RED.id, BLUE.id]);
	});

	it('an OLDER write settling does not release a NEWER write of the same field', async () => {
		// The ordering half, at the hold. Two removes are outstanding; the first
		// one's answer arrives second. Releasing on it would drop the list the
		// user is actually looking at and hand the next edit a stale base — the
		// round-7 defect, reintroduced through the round-8 fix.
		const { onchange, gates } = gatedConsumer('resolve');
		render(FieldEditor, { props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange } });
		await tick();

		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		await tick();
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[1][0]).toEqual([BLUE.id]);
		expect(titles()).toEqual(['Blue']);

		// The FIRST write answers. The prop has not moved (the parent is still
		// waiting on the second), so a release here would show the whole
		// original list under a user who has removed two of it.
		gates[0]();
		await settle();
		expect(titles()).toEqual(['Blue']);

		// The second answers, and the hold is over.
		gates[1]();
		await settle();
		expect(titles()).toEqual(['Green', 'Red', 'Blue']);
	});

	it('CONTROL: a consumer that answers nothing still holds until the prop agrees', async () => {
		// Not every consumer has a server behind it — `CopyItemDialog` sets local
		// state and re-props synchronously. Settlement is an OPTIONAL signal, and
		// a consumer that gives none must keep the round-7 behaviour exactly:
		// releasing on a write whose outcome is unknown is the lost update again.
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange } });
		await tick();

		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		await settle();

		expect(titles()).toEqual(['Red', 'Blue']);
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[1][0]).toEqual([BLUE.id]);
	});

	it('CONTROL: a write that LANDS still releases onto the value the parent wrote', async () => {
		// A CONTROL, and labelled as one after the mutation run said so: disabling
		// the settlement path entirely leaves this leg GREEN, because a write that
		// succeeds also makes the prop AGREE and the round-7 release fires. It is
		// here to show the new path did not break the ordinary success, not as
		// evidence that the new path works — the three legs above are that.
		const gates: Array<() => void> = [];
		let props: Record<string, unknown>;
		const onchange = vi.fn(
			(v: string[]) =>
				new Promise<void>((resolve) => {
					gates.push(() => {
						rerender({ ...editable, value: v, onchange });
						resolve();
					});
				})
		);
		props = { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange };
		const { rerender } = render(FieldEditor, { props });
		await tick();

		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		gates[0]();
		await settle();

		expect(titles()).toEqual(['Red', 'Blue']);
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[1][0]).toEqual([BLUE.id]);
	});
});

describe('multi_relation — what the round-8 enumeration found in the round-8 fix', () => {
	const editable = { field: multi, wsSlug: 'ws', username: 'dave' } as const;

	async function settle() {
		await tick();
		await tick();
		await tick();
	}
	const titles = () => [...document.querySelectorAll('.relation-title')].map((n) => n.textContent);

	it('prop AGREEMENT does not release a write that reports its own settlement', async () => {
		// Remove C, then add C straight back. The second write is holding
		// [Green,Red,Blue] — which EQUALS the prop nobody has changed yet, so the
		// agreement effect read it as "the server confirmed us" and released
		// mid-write. The first write's answer then arrived as [Green,Red] and
		// became the base, and the next removal sent a list with C missing.
		//
		// Agreement is a coincidence test; settlement is the answer. Whichever
		// one is available, only one of them may own the release.
		const gates: Array<() => void> = [];
		// Each write answers when its gate is opened, and answering ALSO lands
		// its own list on the prop — which is what the pane does, and what makes
		// this leg discriminate. An earlier version left the prop alone; with it
		// untouched, a released hold and a held one produce the same next write,
		// so the guard's mutant survived.
		const onchange = vi.fn(
			(v: string[]) =>
				new Promise<void>((resolve) => {
					gates.push(() => {
						rerender({ ...editable, value: v, onchange });
						resolve();
					});
				}),
		);
		const { rerender } = render(FieldEditor, {
			props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange },
		});
		await tick();

		// Remove Blue → [Green,Red] sent.
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[2]);
		await tick();
		expect(onchange.mock.calls[0][0]).toEqual([GREEN.id, RED.id]);

		// Add Blue back → [Green,Red,Blue] sent, which equals the untouched prop.
		await fireEvent.click(buttons(/\+\s*Add/)[0]);
		await tick();
		const blueRow = [...document.querySelectorAll('button')].find((b) =>
			(b.textContent ?? '').includes('Blue'),
		);
		expect(blueRow, 'the picker offered no Blue row to choose').toBeTruthy();
		await fireEvent.click(blueRow!);
		await tick();
		expect(onchange.mock.calls[1][0]).toEqual([GREEN.id, RED.id, BLUE.id]);

		// PRECONDITION: both writes are outstanding and the screen shows the
		// second one's list.
		expect(titles()).toEqual(['Green', 'Red', 'Blue']);

		// The FIRST write answers, putting ITS list — [Green,Red] — on the prop.
		gates[0]();
		await settle();

		// The hold must still be the SECOND write's, so the screen keeps showing
		// Blue and the next edit bases on the list the user is looking at.
		expect(titles()).toEqual(['Green', 'Red', 'Blue']);
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[2][0]).toEqual([RED.id, BLUE.id]);
	});

	it('a consumer that throws SYNCHRONOUSLY does not strand the hold', async () => {
		// The settlement handler is installed AFTER `onchange` returns, so a
		// consumer that throws on the way out armed a hold nothing could ever
		// release — and every later edit took the abandoned list as its base.
		const onchange = vi.fn(() => {
			throw new Error('consumer blew up');
		});
		render(FieldEditor, { props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange } });
		await tick();

		// The component RETHROWS (the error is the consumer's to report, not ours
		// to swallow); where it surfaces from a Svelte event handler is the
		// harness's business and is deliberately not asserted here.
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		await settle();

		// PRECONDITION: it really was called, so "no hold" is not "no write".
		expect(onchange).toHaveBeenCalledTimes(1);
		// The prop never changed, and with no hold the field shows it again.
		expect(titles()).toEqual(['Green', 'Red', 'Blue']);
	});

	it('a RETARGETED editor does not have its hold cleared by the new value', async () => {
		// The agreement effect compared CONTENT and not identity, so an editor
		// reused for another item whose value happened to equal the held list
		// cleared a hold that was never about this field. The stamp exists for
		// exactly this question and the effect was not asking it.
		const onchange = vi.fn();
		const { rerender } = render(FieldEditor, {
			props: { ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange },
		});
		await tick();
		await fireEvent.click(buttons(/^\s*Remove\s*$/)[0]);
		expect(onchange.mock.calls[0][0]).toEqual([RED.id, BLUE.id]);

		// Same instance, different workspace, and the new item's value is the
		// list the OLD workspace's hold is carrying.
		await rerender({ field: multi, wsSlug: 'other-ws', username: 'dave', value: [RED.id, BLUE.id], onchange });
		await tick();
		// Back again. The old hold must still be in force — it was never
		// confirmed by anything.
		await rerender({ ...editable, value: [GREEN.id, RED.id, BLUE.id], onchange });
		await tick();
		expect(titles()).toEqual(['Red', 'Blue']);
	});
});
