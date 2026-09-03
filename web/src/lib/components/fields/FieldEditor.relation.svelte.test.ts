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

const { localIndexMock, localSearchMock, searchApi, collectionStoreMock } = vi.hoisted(() => ({
	localIndexMock: { bootstrapStateFor: vi.fn(), findByIdOrSlug: vi.fn(), getByCollection: vi.fn(), cursorFor: vi.fn() },
	localSearchMock: { search: vi.fn(), epoch: vi.fn() },
	searchApi: vi.fn(),
	collectionStoreMock: { collections: [] as { slug: string }[], collectionsAreFreshFor: vi.fn() },
}));
vi.mock('$lib/api/client', () => ({ api: { search: (...a: unknown[]) => searchApi(...a) } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({ localIndex: localIndexMock }));
vi.mock('$lib/stores/localSearch.svelte', () => ({ localSearch: localSearchMock }));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: collectionStoreMock }));

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
	localSearchMock.search.mockReset().mockReturnValue([]);
	localSearchMock.epoch.mockReset().mockImplementation((ws: string) => epochs.get(ws) ?? 0);
	searchApi.mockReset().mockResolvedValue({ results: [] });
	// Default: the target collection is loaded and live.
	collectionStoreMock.collections = [{ slug: 'colors' }, { slug: 'tasks' }];
	collectionStoreMock.collectionsAreFreshFor.mockReset().mockReturnValue(true);
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
