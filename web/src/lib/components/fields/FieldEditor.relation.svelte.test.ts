// DRAFT pin for U2 (TASK-2868) — written before the relation branch exists,
// per team CONVE-29. Lands as
// web/src/lib/components/fields/FieldEditor.relation.svelte.test.ts
//
// The assertion that fails against TODAY's code is the last one in each leg:
// `fields/FieldEditor.svelte:340-343`'s readonly `{:else}` arm is
// `{value ?? '—'}`, so a relation value renders as a raw UUID in display mode
// and as free text in edit mode.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
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

const LIVE = { id: 'uuid-live', title: 'Red', item_number: 3, collection_prefix: 'COLO', collection_slug: 'colors', slug: 'red', deleted_at: null };
const GONE = { ...LIVE, id: 'uuid-gone', title: 'Retired Blue', item_number: 4, slug: 'retired-blue', deleted_at: '2026-01-01T00:00:00Z' };

const field = { key: 'color', label: 'Colour', type: 'relation' as const, collection: 'colors' };

const rows = new Map<string, unknown>();
const epochs = new SvelteMap<string, number>();

beforeEach(() => {
	rows.clear();
	rows.set(LIVE.id, LIVE);
	rows.set(GONE.id, GONE);
	epochs.clear();
	localIndexMock.bootstrapStateFor.mockReset().mockReturnValue('ready');
	localIndexMock.findByIdOrSlug.mockReset().mockImplementation((_ws: string, id: string) => rows.get(id) ?? null);
	localIndexMock.getByCollection.mockReset().mockReturnValue([LIVE]);
	localIndexMock.cursorFor.mockReset().mockReturnValue('0');
	localIndexMock.pendingResyncFor.mockReset().mockReturnValue(false);
	localSearchMock.search.mockReset().mockReturnValue([]);
	localSearchMock.epoch.mockReset().mockImplementation((ws: string) => epochs.get(ws) ?? 0);
	searchApi.mockReset().mockResolvedValue({ results: [] });
	// Default: the target collection is loaded and live. `tasks` is here so a
	// build that reached for "some collection" rather than the DECLARED target
	// has something wrong to reach for.
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

describe('FieldEditor — relation, three render states', () => {
	it('(a) a live target renders a chip that opens it', async () => {
		render(FieldEditor, {
			props: { field, value: LIVE.id, wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} },
		});
		await tick();
		expect(document.body.textContent).toContain('COLO-3');
		expect(document.body.textContent).toContain('Red');
		const a = document.querySelector('a.relation-chip');
		expect(a).not.toBeNull();
		expect(a!.getAttribute('href')).toBe('/dave/ws/colors/COLO-3');
		expectNoBareUuid();
	});

	it('(a2) a live target with no route still names the item, never the id', async () => {
		// `username` is what builds the href, and `CopyItemDialog` has none. The
		// chip degrades to a non-link — it must NOT degrade to the raw value,
		// which is what the old readonly arm did.
		render(FieldEditor, { props: { field, value: LIVE.id, wsSlug: 'ws', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.querySelector('a.relation-chip')).toBeNull();
		expect(document.body.textContent).toContain('COLO-3');
		expect(document.body.textContent).toContain('Red');
		expectNoBareUuid();
	});

	it('(b) a soft-deleted target renders honestly as deleted', async () => {
		render(FieldEditor, { props: { field, value: GONE.id, wsSlug: 'ws', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.body.textContent).toMatch(/\(deleted\)/i);
		expectNoBareUuid();
	});

	it('(c) a value resolving to nothing renders as unresolved, NOT as deleted', async () => {
		// This is what R1 + R2 have been writing into these fields all along, so
		// it is the common case on existing data, not an edge. It must be
		// distinguishable from (b) — a value whose target was deleted and a value
		// that was never an id are different facts about the item.
		render(FieldEditor, { props: { field, value: 'red', wsSlug: 'ws', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.body.textContent).not.toMatch(/\(deleted\)/i);
		expect(document.body.textContent ?? '').toMatch(/unresolved|not found|unknown/i);
		expectNoBareUuid();
	});
});

describe('FieldEditor — relation resolves by ID, in the declared collection', () => {
	// Both legs found by driving this in a real browser, not by reading the code.

	it('a legacy free-text value that matches an item SLUG is unresolved, not a chip', async () => {
		// `localIndex.findByIdOrSlug` resolves by id OR slug, so the string "red"
		// — exactly what the old text fallback wrote into these fields — otherwise
		// renders as a working reference to the item slugged "red". The field
		// stores an ID; a slug match makes the chip lie about what is stored, and
		// slugs are mutable, so the same value could point elsewhere tomorrow.
		rows.set('red', { ...LIVE, id: 'uuid-live' });
		localIndexMock.findByIdOrSlug.mockImplementation((_ws: string, k: string) =>
			k === 'red' ? LIVE : (rows.get(k) ?? null)
		);
		render(FieldEditor, { props: { field, value: 'red', wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.body.textContent).toMatch(/unresolved/i);
		expect(document.body.textContent).not.toContain('Red');
	});

	it('an id resolving into a DIFFERENT collection is unresolved', async () => {
		// The helper is workspace-wide. Without the collection check a relation
		// declared against `colors` renders an item from `tasks`. Same defect the
		// design pass recorded against the server's `ResolveItem`.
		const foreign = { ...LIVE, id: 'uuid-foreign', title: 'A Task', collection_slug: 'tasks', collection_prefix: 'TASK' };
		rows.set(foreign.id, foreign);
		render(FieldEditor, { props: { field, value: foreign.id, wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.body.textContent).toMatch(/unresolved/i);
		expect(document.body.textContent).not.toContain('A Task');
	});
});

describe('FieldEditor — a renamed target collection must not look like data loss', () => {
	// `FieldDef.collection` holds a SLUG, and renaming a collection changes its
	// slug without migrating the relation definitions pointing at it — nothing
	// in `store.UpdateCollection` touches them (codex round 1 P1 on this unit).
	// The collection check added for cross-collection resolution would then
	// report EVERY stored value as unresolved, which is a schema problem
	// presenting as lost data.

	it('still renders the chip when the declared target no longer exists', async () => {
		// Faithful to what a rename actually does: `localIndex.applyRetag` moves
		// the indexed ROWS onto the new slug, while `field.collection` keeps
		// pointing at the old one. The row and the field therefore DISAGREE — and
		// modelling only the store's collection list (as a first draft of this
		// test did) leaves them agreeing, so the mutant that makes the collection
		// check unconditional survives.
		collectionStoreMock.collections = [{ slug: 'colours-renamed' }]; // `colors` is gone
		const retagged = { ...LIVE, collection_slug: 'colours-renamed', collection_prefix: 'COLO' };
		rows.set(retagged.id, retagged);
		render(FieldEditor, { props: { field, value: retagged.id, wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.body.textContent).toContain('Red');
		expect(document.body.textContent).not.toMatch(/unresolved/i);
	});

	it('survives the window where the index is retagged but the collection list is not', async () => {
		// codex round 2 P1. `retagCollection` moves the ROWS immediately;
		// `loadCollections` is fired with `void` — not awaited, rejection
		// swallowed. So there is a window where the list still holds the OLD slug
		// (so the target reads 'live') while the row already carries the NEW one.
		// Judging the mismatch there reports the value as unresolved, and if that
		// refetch FAILS the state is permanent, not transient.
		collectionStoreMock.collections = [{ slug: 'colors' }]; // stale list: pre-rename
		const retagged = { ...LIVE, collection_slug: 'colours-renamed' };
		rows.set(retagged.id, retagged);
		render(FieldEditor, { props: { field, value: retagged.id, wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.body.textContent).toContain('Red');
		expect(document.body.textContent).not.toMatch(/unresolved/i);
	});

	it('CONTROL: a genuine cross-collection value is still rejected when both slugs are known', async () => {
		// The leg that stops the two guards above from being "never judge". Both
		// `colors` and `tasks` are live, so the list and the index agree and the
		// mismatch IS evidence.
		collectionStoreMock.collections = [{ slug: 'colors' }, { slug: 'tasks' }];
		const foreign = { ...LIVE, id: 'uuid-foreign2', title: 'A Task', collection_slug: 'tasks' };
		rows.set(foreign.id, foreign);
		render(FieldEditor, { props: { field, value: foreign.id, wsSlug: 'ws', username: 'dave', readonly: true, onchange: () => {} } });
		await tick();
		expect(document.body.textContent).toMatch(/unresolved/i);
		expect(document.body.textContent).not.toContain('A Task');
	});

	it('goes read-only rather than offering a picker that can never match', async () => {
		collectionStoreMock.collections = [{ slug: 'colours-renamed' }];
		render(FieldEditor, { props: { field, value: '', wsSlug: 'ws', username: 'dave', readonly: false, onchange: () => {} } });
		await tick();
		expect(document.querySelector('.picker-input')).toBeNull();
	});

	it('treats a not-yet-loaded collection list as unknown, not as stale', async () => {
		// Absence of evidence. Reading it as stale would flash every relation
		// field into read-only on first paint, before the collection list lands.
		//
		// Asserted on EDITABILITY, not on the chip: a stale target also renders
		// the chip, so a chip assertion cannot tell the two apart.
		collectionStoreMock.collectionsAreFreshFor.mockReturnValue(false);
		render(FieldEditor, { props: { field, value: '', wsSlug: 'ws', username: 'dave', readonly: false, onchange: () => {} } });
		await tick();
		expect(document.querySelector('.picker-input')).not.toBeNull();
	});
});

describe('FieldEditor — relation, the gate', () => {
	it('renders no picker without a wsSlug — the CopyItemDialog call site', async () => {
		render(FieldEditor, { props: { field, value: '', readonly: false, onchange: () => {} } });
		await tick();
		expect(document.querySelector('.picker-input')).toBeNull();
	});

	it('renders no picker without a target collection', async () => {
		const { collection: _drop, ...untargeted } = field;
		render(FieldEditor, { props: { field: untargeted, value: '', wsSlug: 'ws', readonly: false, onchange: () => {} } });
		await tick();
		expect(document.querySelector('.picker-input')).toBeNull();
	});

	it('a set relation shows the value, not a permanently-open search box', async () => {
		// The first browser pass rendered the chip, the picker input still holding
		// the query, and the result list still showing the row just chosen — the
		// same item three times, under every relation field on the page.
		render(FieldEditor, { props: { field, value: LIVE.id, wsSlug: 'ws', username: 'dave', readonly: false, onchange: () => {} } });
		await tick();
		expect(document.querySelector('.relation-chip')).not.toBeNull();
		expect(document.querySelector('.picker-input')).toBeNull();
	});

	it('Change reopens the picker; choosing closes it and emits the new id', async () => {
		const onchange = vi.fn();
		render(FieldEditor, { props: { field, value: LIVE.id, wsSlug: 'ws', username: 'dave', readonly: false, onchange } });
		await tick();

		const change = [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === 'Change');
		expect(change, 'no Change control').toBeDefined();
		change!.click();
		await tick();
		expect(document.querySelector('.picker-input')).not.toBeNull();

		const option = document.querySelector<HTMLElement>('[role="option"]');
		expect(option, 'picker listed nothing to choose').not.toBeNull();
		option!.click();
		await tick();

		expect(onchange).toHaveBeenCalledTimes(1);
		expect(onchange.mock.calls[0][0]).toBe(LIVE.id);
		// And it CLOSES. Without this the mutant that drops the reset survives:
		// emitting the value while leaving the search box open is the behaviour
		// the browser pass rejected, and asserting only the emit misses it.
		expect(document.querySelector('.picker-input')).toBeNull();
		expect(document.querySelector('.relation-chip')).not.toBeNull();
	});

	it('Clear empties the field', async () => {
		const onchange = vi.fn();
		render(FieldEditor, { props: { field, value: LIVE.id, wsSlug: 'ws', username: 'dave', readonly: false, onchange } });
		await tick();
		const clear = [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === 'Clear');
		expect(clear, 'no Clear control').toBeDefined();
		clear!.click();
		expect(onchange).toHaveBeenCalledWith('');
	});

	it('CONTROL: with both, the editable branch mounts the picker scoped to the target', async () => {
		render(FieldEditor, { props: { field, value: '', wsSlug: 'ws', readonly: false, onchange: () => {} } });
		await tick();
		expect(document.querySelector('.picker-input')).not.toBeNull();
		expect(localIndexMock.getByCollection).toHaveBeenCalledWith('ws', 'colors');
	});
});


// ── U8: inline create from the relation picker (TASK-2877) ───────────────
//
// Pins written before the wiring exists, per team CONVE-29.
//
// `ItemPicker` owns the ROW (suite in ItemPicker.svelte.test.ts). What is
// pinned here is everything the picker deliberately does not know: which
// collection the item lands in, whether this user may put one there, and the
// upsert that makes the no-duplicate suppression true on the next keystroke.
describe('FieldEditor — relation, inline create (PLAN-2857 U8)', () => {
	const editableProps = {
		field,
		value: '',
		wsSlug: 'ws',
		username: 'dave',
		onchange: () => {},
	};

	function createRow(): HTMLElement | null {
		return document.querySelector<HTMLElement>('.picker-create');
	}

	async function typeQuery(value: string) {
		const el = document.querySelector<HTMLInputElement>('.picker-input');
		if (!el) throw new Error('.picker-input not found');
		el.value = value;
		el.dispatchEvent(new Event('input', { bubbles: true }));
		await tick();
	}

	it('offers no create row to a user who cannot create in the TARGET collection', async () => {
		// The gate is collection-level `canEditCollection` — the "+ New"
		// predicate — asked about the target, NOT about wherever the item being
		// edited lives. A viewer with edit rights on this item and none on
		// `colors` still gets no create row.
		workspaceStoreMock.canEditCollection.mockReturnValue(false);
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		render(FieldEditor, { props: editableProps });
		await tick();

		await typeQuery('Purple');

		expect(createRow()).toBeNull();
		expect(workspaceStoreMock.canEditCollection).toHaveBeenCalledWith('coll-colors');
	});

	it('CONTROL: the same query offers the row when the user CAN create there', async () => {
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		render(FieldEditor, { props: editableProps });
		await tick();

		await typeQuery('Purple');

		expect(createRow()).not.toBeNull();
		// Named by its display name, not its slug.
		expect(createRow()!.textContent).toContain('Colors');
	});

	it('creates in the TARGET collection and sets the field to the new id', async () => {
		// The unit's proving test, first leg. The mutant it kills is creating in
		// any collection other than the field's declared target — `tasks` is in
		// the store precisely so that mutant has somewhere to land.
		const created = {
			id: 'uuid-new',
			title: 'Purple',
			item_number: 9,
			collection_prefix: 'COLO',
			collection_slug: 'colors',
			slug: 'purple',
			deleted_at: null,
		};
		createApi.mockResolvedValue(created);
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editableProps, onchange } });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await vi.waitFor(() => expect(onchange).toHaveBeenCalled());

		expect(createApi).toHaveBeenCalledTimes(1);
		const [ws, coll, body] = createApi.mock.calls[0] as [string, string, { title: string }];
		expect(ws).toBe('ws');
		expect(coll).toBe('colors');
		expect(body.title).toBe('Purple');
		expect(onchange).toHaveBeenCalledWith('uuid-new');
	});

	it('sends no field values, so the server applies the collection schema defaults', async () => {
		// `items.ValidateFields` fills every missing key that declares a
		// `Default` and the create handler marshals the DEFAULTED map back
		// (internal/server/handlers_items.go, "Marshal validated/defaulted
		// fields back"). Guessing a status here — e.g. the first `options`
		// entry, as the collection page's "+ New" does — would override that
		// answer with a worse one, and would be wrong for any schema whose
		// default is not its first option.
		createApi.mockResolvedValue({ id: 'uuid-new', title: 'Purple', collection_slug: 'colors' });
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		render(FieldEditor, { props: editableProps });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await vi.waitFor(() => expect(createApi).toHaveBeenCalled());

		const body = (createApi.mock.calls[0] as [string, string, Record<string, unknown>])[2];
		expect(body.fields).toBeUndefined();
	});

	it('upserts the new row into the local index, under the epoch captured before the call', async () => {
		// This is what makes U8's second leg true: the picker suppresses its
		// create row when an exact title is already in `rawResults`, and
		// `rawResults` comes from the index. Without this upsert the row is
		// invisible to the very next keystroke and the same text offers to
		// create a SECOND item.
		//
		// The epoch is BUG-2098's guard, and it has to be read before the
		// request, not after: a projection resync landing mid-flight means the
		// response was authorized under a scope that no longer applies.
		const created = { id: 'uuid-new', title: 'Purple', collection_slug: 'colors' };
		createApi.mockImplementation(async () => {
			localIndexMock.scopeEpochFor.mockReturnValue(99);
			return created;
		});
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		render(FieldEditor, { props: editableProps });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await vi.waitFor(() => expect(localIndexMock.upsert).toHaveBeenCalled());

		expect(localIndexMock.upsert).toHaveBeenCalledWith('ws', created, 7);
	});

	it('a second pass at the same text offers the existing row and no create', async () => {
		// The proving test's second leg, driven through the state the first one
		// leaves behind: the created row is now in the index, so the picker
		// answers with it and withholds the affordance entirely. There is no
		// create-time uniqueness check to rely on, and this is why one is not
		// needed.
		const created = {
			id: 'uuid-new',
			title: 'Purple',
			item_number: 9,
			collection_prefix: 'COLO',
			collection_slug: 'colors',
			slug: 'purple',
			deleted_at: null,
		};
		rows.set(created.id, created);
		localIndexMock.getByCollection.mockReturnValue([created]);
		localSearchMock.search.mockReturnValue([{ id: created.id, score: 1 }]);
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editableProps, onchange } });
		await tick();

		await typeQuery('Purple');

		expect(createRow()).toBeNull();
		const row = document.querySelector<HTMLElement>('.picker-result');
		expect(row).not.toBeNull();
		row!.click();
		await tick();
		expect(createApi).not.toHaveBeenCalled();
		expect(onchange).toHaveBeenCalledWith('uuid-new');
	});

	it('a failed create surfaces the error and leaves the field alone', async () => {
		// A required field with no schema default makes the server 400 here, so
		// this is a reachable path and not a hypothetical. The value must not
		// move, and the query must survive so the user can retry or pick
		// something else.
		createApi.mockRejectedValue(new Error('field "shade" is required'));
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editableProps, onchange } });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await vi.waitFor(() => expect(toastMock.show).toHaveBeenCalled());

		expect(toastMock.show.mock.calls[0][0]).toContain('shade');
		expect(onchange).not.toHaveBeenCalled();
		expect(localIndexMock.upsert).not.toHaveBeenCalled();
		expect(document.querySelector<HTMLInputElement>('.picker-input')!.value).toBe('Purple');
	});

	it('a create that lands after the user picked another row does not overwrite it', async () => {
		// codex round 1 P1. The create is a round trip and the picker stays open
		// through it, so the user can settle on a different row before it
		// resolves. Last write wins is the WRONG rule here: the later write is
		// the user's explicit choice and the earlier one is a promise they have
		// already moved past.
		let release!: (v: unknown) => void;
		createApi.mockReturnValue(new Promise((r) => { release = r; }));
		const picked = {
			id: 'uuid-picked', title: 'Teal', item_number: 5, collection_prefix: 'COLO',
			collection_slug: 'colors', slug: 'teal', deleted_at: null,
		};
		rows.set(picked.id, picked);
		localIndexMock.getByCollection.mockReturnValue([picked]);
		localSearchMock.search.mockReturnValue([]);
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editableProps, onchange } });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await tick();

		// The user settles on an existing row while the create is in flight.
		localSearchMock.search.mockReturnValue([{ id: picked.id, score: 1 }]);
		await typeQuery('Teal');
		document.querySelector<HTMLElement>('.picker-result')!.click();
		await tick();
		expect(onchange).toHaveBeenCalledWith('uuid-picked');

		release({ id: 'uuid-new', title: 'Purple', collection_slug: 'colors' });
		await tick();
		await tick();

		// The created item is REAL and belongs in the index either way — it just
		// must not become the field's value.
		expect(localIndexMock.upsert).toHaveBeenCalled();
		expect(onchange).toHaveBeenCalledTimes(1);
		expect(onchange).not.toHaveBeenCalledWith('uuid-new');
	});

	it('a create that lands after this editor is destroyed writes nothing', async () => {
		// codex round 1 P1, the other half. ItemDetail's fields section is
		// `{#key itemSlug}`-remounted on an item switch, so a switch DESTROYS
		// this component — but not the in-flight promise, and `onchange` calls
		// into the persistent parent, whose `updateField` builds its PATCH
		// against whatever item is current at CALL time. An unfenced completion
		// therefore writes the new colour onto a DIFFERENT car.
		let release!: (v: unknown) => void;
		createApi.mockReturnValue(new Promise((r) => { release = r; }));
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editableProps, onchange } });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await tick();

		cleanup(); // the {#key itemSlug} remount
		release({ id: 'uuid-new', title: 'Purple', collection_slug: 'colors' });
		await tick();
		await tick();

		expect(onchange).not.toHaveBeenCalled();
	});

	it('a create that lands after the user escaped out of the picker writes nothing', async () => {
		// codex round 2 P1. Backing out is as explicit a choice as picking a
		// different row, and it was not fenced: `oncancel` only closed the
		// picker. The pending create then resolved and selected an item the user
		// had just declined.
		let release!: (v: unknown) => void;
		createApi.mockReturnValue(new Promise((r) => { release = r; }));
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		const onchange = vi.fn();
		// An EXISTING value, because that is when the picker is given an
		// `oncancel` at all — with an empty relation there is nothing to cancel
		// back to.
		render(FieldEditor, { props: { ...editableProps, value: LIVE.id, onchange } });
		await tick();
		document.querySelector<HTMLButtonElement>('.relation-action')!.click(); // "Change"
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await tick();

		// Escape clears the query, then escapes the picker.
		const el = document.querySelector<HTMLInputElement>('.picker-input')!;
		el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
		await tick();
		el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
		await tick();

		release({ id: 'uuid-new', title: 'Purple', collection_slug: 'colors' });
		await tick();
		await tick();

		expect(onchange).not.toHaveBeenCalled();
	});

	it('a create that lands after a workspace switch writes nothing', async () => {
		// codex round 2 P1. `ItemDetail`'s fields subtree is keyed on `itemSlug`
		// ALONE, so switching workspaces to an item carrying the SAME ref —
		// every workspace has a TASK-5 — does not remount this component and
		// leaves `destroyed` false. Without comparing the captured workspace to
		// the current one, the completion writes an item ID from the previous
		// workspace into the new workspace's item.
		let release!: (v: unknown) => void;
		createApi.mockReturnValue(new Promise((r) => { release = r; }));
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		const onchange = vi.fn();
		const { rerender } = render(FieldEditor, { props: { ...editableProps, onchange } });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await tick();

		await rerender({ ...editableProps, wsSlug: 'other-ws', onchange });
		await tick();

		release({ id: 'uuid-new', title: 'Purple', collection_slug: 'colors' });
		await tick();
		await tick();

		expect(onchange).not.toHaveBeenCalled();
		// The row still belongs in the workspace it was created in — the fence
		// is about where the VALUE is written, not about hiding a real item.
		expect(localIndexMock.upsert).toHaveBeenCalledWith('ws', expect.anything(), 7);
	});

	it('a failed create the user already escaped out of surfaces no toast', async () => {
		// codex round 4 P2. The success path was fenced and the failure path was
		// not, so a create the user backed out of still threw an error over
		// whatever they moved on to.
		let reject!: (e: unknown) => void;
		createApi.mockReturnValue(new Promise((_r, rj) => { reject = rj; }));
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		render(FieldEditor, { props: { ...editableProps, value: LIVE.id } });
		await tick();
		document.querySelector<HTMLButtonElement>('.relation-action')!.click();
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await tick();

		const el = document.querySelector<HTMLInputElement>('.picker-input')!;
		el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
		await tick();
		el.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
		await tick();

		reject(new Error('field "shade" is required'));
		await tick();
		await tick();

		expect(toastMock.show).not.toHaveBeenCalled();
	});

	it('a create that lands after the workspace was PURGED writes nothing and upserts nothing', async () => {
		// codex round 6. The epoch check alone cannot see a drop: `reset()`
		// deletes the state and the replacement starts at `scopeEpoch` 0 — which
		// is also the value whenever no resync has ever happened, i.e. almost
		// always. So equality passes across exactly the event it was meant to
		// catch. `resetGenerationFor` is the identity signal, and it outlives
		// the state it counts.
		//
		// The upsert matters as much as the link here: a brand-new id was never
		// in `upsert`'s fenced set (nothing to fence — the row did not exist
		// when the purge ran), so without this the purged workspace gets a row
		// written back into it and persisted to IDB.
		let release!: (v: unknown) => void;
		createApi.mockReturnValue(new Promise((r) => { release = r; }));
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editableProps, onchange } });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await tick();

		// The purge, with the epoch landing back where it started — the common
		// case, not a coincidence.
		localIndexMock.resetGenerationFor.mockReturnValue(4);
		localIndexMock.scopeEpochFor.mockReturnValue(7);
		release({ id: 'uuid-new', title: 'Purple', collection_slug: 'colors' });
		await tick();
		await tick();

		expect(onchange).not.toHaveBeenCalled();
		expect(localIndexMock.upsert).not.toHaveBeenCalled();
	});

	it('a failed create whose workspace was purged surfaces no toast', async () => {
		// codex round 6 P2: the failure path carried a copy of SOME of the
		// success path's conditions and had drifted. Both now ask one predicate.
		let reject!: (e: unknown) => void;
		createApi.mockReturnValue(new Promise((_r, rj) => { reject = rj; }));
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		render(FieldEditor, { props: editableProps });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await tick();

		localIndexMock.resetGenerationFor.mockReturnValue(4);
		reject(new Error('boom'));
		await tick();
		await tick();

		expect(toastMock.show).not.toHaveBeenCalled();
	});

	it('a create that lands after the workspace index was reset writes nothing', async () => {
		// codex round 5 P1. `localIndex.reset()` DELETES the workspace state and
		// the next bootstrap starts a fresh one at `scopeEpoch` 0, so a captured
		// epoch of 7 is NOT below the current 0 and sails through `upsert`'s
		// one-sided guard — linking a row minted under an identity that no
		// longer holds. Equality is what catches the downward jump.
		let release!: (v: unknown) => void;
		createApi.mockReturnValue(new Promise((r) => { release = r; }));
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		const onchange = vi.fn();
		render(FieldEditor, { props: { ...editableProps, onchange } });
		await tick();

		await typeQuery('Purple');
		createRow()!.click();
		await tick();

		// The purge: state deleted, re-bootstrapped, epoch back to 0.
		localIndexMock.scopeEpochFor.mockReturnValue(0);
		release({ id: 'uuid-new', title: 'Purple', collection_slug: 'colors' });
		await tick();
		await tick();

		expect(onchange).not.toHaveBeenCalled();
	});

	it('offers no create row while the collection list is not fresh for this workspace', async () => {
		// codex round 1 P2. `collectionStore.collections` is a single global
		// list, so during a workspace switch it still holds the PREVIOUS
		// workspace's rows. A slug match against those yields another
		// workspace's collection ID, and gating permission on it is asking the
		// wrong question — the same freshness gate `relationTarget` already
		// applies, which this derivation was missing.
		collectionStoreMock.collectionsAreFreshFor.mockReturnValue(false);
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		render(FieldEditor, { props: editableProps });
		await tick();
		// A query is required or this asserts nothing: an EMPTY query never
		// offers a create row, so the leg would pass against the ungated build.
		await typeQuery('Purple');

		expect(createRow()).toBeNull();
	});

	it('a target the collection list does not name renders read-only — there is no picker to create from', async () => {
		// Worth stating rather than asserting a bare absence. When the declared
		// target is absent from a FRESH list it is a renamed collection, and
		// `relationEditable` already refuses to mount a picker at all (a picker
		// scoped to a slug nothing matches would list nothing, forever). So the
		// create row is unreachable one layer ABOVE the permission gate, and a
		// test that typed a query here would fail on a missing input rather than
		// on the behaviour it names.
		//
		// The permission gate's own "no collection ID" branch is exercised by
		// the not-fresh leg above, where the picker DOES mount.
		collectionStoreMock.collections = [{ id: 'coll-tasks', slug: 'tasks', name: 'Tasks' }];
		localIndexMock.getByCollection.mockReturnValue([]);
		localSearchMock.search.mockReturnValue([]);
		render(FieldEditor, { props: editableProps });
		await tick();

		expect(document.querySelector('.picker-input')).toBeNull();
		expect(createRow()).toBeNull();
	});
});
