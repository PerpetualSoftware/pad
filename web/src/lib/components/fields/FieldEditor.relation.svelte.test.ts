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

const { localIndexMock, localSearchMock, searchApi } = vi.hoisted(() => ({
	localIndexMock: { bootstrapStateFor: vi.fn(), findByIdOrSlug: vi.fn(), getByCollection: vi.fn(), cursorFor: vi.fn() },
	localSearchMock: { search: vi.fn(), epoch: vi.fn() },
	searchApi: vi.fn(),
}));
vi.mock('$lib/api/client', () => ({ api: { search: (...a: unknown[]) => searchApi(...a) } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({ localIndex: localIndexMock }));
vi.mock('$lib/stores/localSearch.svelte', () => ({ localSearch: localSearchMock }));

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

	it('CONTROL: with both, the editable branch mounts the picker scoped to the target', async () => {
		render(FieldEditor, { props: { field, value: '', wsSlug: 'ws', readonly: false, onchange: () => {} } });
		await tick();
		expect(document.querySelector('.picker-input')).not.toBeNull();
		expect(localIndexMock.getByCollection).toHaveBeenCalledWith('ws', 'colors');
	});
});
