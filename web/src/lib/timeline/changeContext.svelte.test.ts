import { describe, it, expect, vi, beforeEach } from 'vitest';

// BUG-2872 — the context a page hands its timeline: relation values resolve
// id-only and scoped to the field's declared target collection (the relation
// family's narrowRelationRow), and "ready" is the workspace index's state.
const { localIndexMock, collectionStoreMock } = vi.hoisted(() => ({
	localIndexMock: { bootstrapStateFor: vi.fn(), findByIdOrSlug: vi.fn() },
	collectionStoreMock: { collections: [{ id: 'c1', slug: 'people' }, { id: 'c2', slug: 'tasks' }] },
}));
vi.mock('$lib/stores/localIndex.svelte', () => ({ localIndex: localIndexMock }));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: collectionStoreMock }));

import { createChangeContext } from './changeContext';

const ID = '11111111-2222-3333-4444-555555555555';
const row = (over: Record<string, unknown> = {}) => ({ id: ID, slug: 'ada', title: 'Ada', collection_slug: 'people', ...over });

describe('createChangeContext (BUG-2872)', () => {
	beforeEach(() => vi.clearAllMocks());

	it('resolves an id in the declared collection', () => {
		localIndexMock.findByIdOrSlug.mockReturnValue(row());
		const ctx = createChangeContext(() => 'ws', () => undefined);
		expect(ctx.resolveRow(ID, 'people')?.title).toBe('Ada');
		expect(localIndexMock.findByIdOrSlug).toHaveBeenCalledWith('ws', ID);
	});

	it('does NOT take a row from another collection than the field declares', () => {
		localIndexMock.findByIdOrSlug.mockReturnValue(row({ collection_slug: 'tasks' }));
		const ctx = createChangeContext(() => 'ws', () => undefined);
		expect(ctx.resolveRow(ID, 'people')).toBeNull();
	});

	it('does NOT resolve by slug (a relation stores an id)', () => {
		localIndexMock.findByIdOrSlug.mockReturnValue(row());
		const ctx = createChangeContext(() => 'ws', () => undefined);
		expect(ctx.resolveRow('ada', 'people')).toBeNull();
	});

	it('is ready only when the workspace index is', () => {
		const ctx = createChangeContext(() => 'ws', () => undefined);
		localIndexMock.bootstrapStateFor.mockReturnValue('cold');
		expect(ctx.indexReady()).toBe(false);
		localIndexMock.bootstrapStateFor.mockReturnValue('ready');
		expect(ctx.indexReady()).toBe(true);
		expect(createChangeContext(() => '', () => undefined).indexReady()).toBe(false);
	});
});
