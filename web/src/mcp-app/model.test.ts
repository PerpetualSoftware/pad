import { describe, expect, it } from 'vitest';
import { CARD_ACTIONS, isItemResult, shouldRenderCard, editableFields, buildUpdateArgs, parseToolError, contentPreview } from './model';

const item = { id: 'uuid', title: 'Fix bug', seq: 42, collection_slug: 'tasks', fields: { status: 'open' }, content: 'Body' };

describe('item model', () => {
	it('accepts an item and rejects every missing or wrongly typed required key', () => {
		expect(isItemResult(item)).toBe(true);
		for (const key of ['id', 'title', 'seq', 'collection_slug'] as const) {
			const missing: Record<string, unknown> = { ...item }; delete missing[key];
			expect(isItemResult(missing)).toBe(false);
			for (const value of [null, {}, [], true, key === 'seq' ? '42' : 42]) expect(isItemResult({ ...item, [key]: value })).toBe(false);
		}
		for (const value of [null, [], 'item', 1]) expect(isItemResult(value)).toBe(false);
	});
	it('renders item results only for card actions', () => {
		expect(CARD_ACTIONS).toEqual(['get', 'create', 'update', 'move', 'restore', 'claim', 'release']);
		for (const action of CARD_ACTIONS) expect(shouldRenderCard({ action }, item)).toBe(true);
		for (const action of ['list', 'search', 'delete', undefined]) expect(shouldRenderCard({ action }, item)).toBe(false);
		expect(shouldRenderCard(undefined, item)).toBe(false);
		for (const content of [null, {}, [item], { items: [item] }]) expect(shouldRenderCard({ action: 'get' }, content)).toBe(false);
	});
	it('exposes only non-computed selects with options and current values', () => {
		const schema = { fields: [
			{ key: 'status', type: 'select', options: ['open', 'done'] },
			{ key: 'missing', type: 'select', options: ['x'], computed: false },
			{ key: 'derived', type: 'select', computed: true, options: ['x'] },
			{ key: 'tags', type: 'multi_select', options: ['x'] },
			{ key: 'notes', type: 'text' }
		] };
		expect(editableFields(schema, item)).toEqual([
			{ key: 'status', type: 'select', options: ['open', 'done'], value: 'open' },
			{ key: 'missing', type: 'select', options: ['x'], computed: false, value: undefined }
		]);
	});
	it('builds guarded metadata-only updates across input combinations', () => {
		for (const workspace of [undefined, '', 'demo']) for (const title of [undefined, '', 'New title']) for (const field of [undefined, { key: 'status', value: 'done' }]) for (const expectedSeq of [0, 1, 42]) {
			const result = buildUpdateArgs({ workspace, ref: 'TASK-1', title, field, expectedSeq });
			expect(result).toEqual({ action: 'update', ref: 'TASK-1', expected_seq: expectedSeq, ...(workspace !== undefined ? { workspace } : {}), ...(title !== undefined ? { title } : {}), ...(field ? { field: ['status=done'] } : {}) });
			for (const forbidden of ['content', 'stdin', 'content_transform']) expect(result).not.toHaveProperty(forbidden);
		}
	});
	it('parses conflict envelopes from structured content and text fallback', () => {
		const error = { code: 'update_conflict', message: 'Item changed' };
		expect(parseToolError({ isError: true, structuredContent: { error } })).toEqual(error);
		expect(parseToolError({ isError: true, content: [{ type: 'text', text: JSON.stringify({ error }) }] })).toEqual(error);
		expect(parseToolError({ isError: true, content: [{ type: 'text', text: 'Failure' }] })).toEqual({ code: 'unknown', message: 'Failure' });
		expect(parseToolError({ structuredContent: { error } })).toBeUndefined();
	});
	it('returns plain text with a bounded preview', () => {
		expect(contentPreview(undefined)).toBe('');
		expect(contentPreview('short')).toBe('short');
		expect(contentPreview('x'.repeat(400))).toBe('x'.repeat(400));
		expect(contentPreview('x'.repeat(401))).toBe('x'.repeat(400) + '…');
		expect(contentPreview('abcdef', 3)).toBe('abc…');
	});
});
