import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { FieldDef } from '$lib/types';

// BUG-2872 — one side of an activity field change. A relation side renders the
// target (or an honest state), never the item ID the field stores; any other
// field's side renders its text untouched.
const { localIndexMock, collectionStoreMock } = vi.hoisted(() => ({
	localIndexMock: { bootstrapStateFor: vi.fn(), findByIdOrSlug: vi.fn() },
	collectionStoreMock: { collections: [{ id: 'c-people', slug: 'people' }, { id: 'c-tasks', slug: 'tasks' }] },
}));
vi.mock('$lib/stores/localIndex.svelte', () => ({ localIndex: localIndexMock }));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: collectionStoreMock }));

import ActivityChangeValue from './ActivityChangeValue.svelte';

const ID = '11111111-2222-3333-4444-555555555555';
const OWNER: FieldDef = { key: 'owner', label: 'Owner', type: 'relation', collection: 'people' } as FieldDef;
const NOTE: FieldDef = { key: 'note', label: 'Note', type: 'text' } as FieldDef;
const row = (over: Record<string, unknown> = {}) => ({
	id: ID,
	slug: 'ada',
	title: 'Ada Lovelace',
	collection_slug: 'people',
	collection_prefix: 'PEOP',
	item_number: 7,
	...over,
});

function textOf(props: { text: string; field?: FieldDef | null }) {
	const { container } = render(ActivityChangeValue, { props: { wsSlug: 'ws', ...props } });
	return container.textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

describe('ActivityChangeValue (BUG-2872)', () => {
	beforeEach(() => {
		localIndexMock.bootstrapStateFor.mockReturnValue('ready');
		localIndexMock.findByIdOrSlug.mockReturnValue(null);
	});
	afterEach(() => {
		cleanup();
		vi.clearAllMocks();
	});

	it('a resolved relation renders REF and title, never the id', () => {
		localIndexMock.findByIdOrSlug.mockImplementation((_ws: string, v: string) => (v === ID ? row() : null));
		const t = textOf({ text: ID, field: OWNER });
		expect(t).toContain('Ada Lovelace');
		expect(t).toMatch(/PEOP-7/);
		expect(t).not.toContain(ID);
	});

	it('a deleted target says so', () => {
		localIndexMock.findByIdOrSlug.mockReturnValue(row({ deleted_at: '2026-09-01T00:00:00Z' }));
		const t = textOf({ text: ID, field: OWNER });
		expect(t).toContain('(deleted)');
		expect(t).not.toContain(ID);
	});

	it('with the index READY, a value naming nothing is "Unresolved reference"', () => {
		const t = textOf({ text: ID, field: OWNER });
		expect(t).toBe('Unresolved reference');
	});

	it('with the index NOT ready, a miss is "Linked item" — not a claim that it names nothing', () => {
		localIndexMock.bootstrapStateFor.mockReturnValue('cold');
		const t = textOf({ text: ID, field: OWNER });
		expect(t).toBe('Linked item');
		expect(t).not.toContain(ID);
	});

	it('a row in ANOTHER collection than the field declares is not taken for the target', () => {
		localIndexMock.findByIdOrSlug.mockReturnValue(row({ collection_slug: 'tasks', collection_prefix: 'TASK' }));
		expect(textOf({ text: ID, field: OWNER })).toBe('Unresolved reference');
	});

	it('a non-relation field renders its text verbatim, even an id-shaped one', () => {
		localIndexMock.findByIdOrSlug.mockReturnValue(row());
		expect(textOf({ text: ID, field: NOTE })).toBe(ID);
		expect(textOf({ text: 'plain words', field: null })).toBe('plain words');
	});

	it('an empty relation side stays empty', () => {
		expect(textOf({ text: '', field: OWNER })).toBe('');
	});
});
