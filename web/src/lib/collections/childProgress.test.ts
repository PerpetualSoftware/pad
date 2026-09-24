import { describe, expect, it } from 'vitest';
import { DEFAULT_ABANDONED_STATUSES, getAbandonedOptions, type Collection, type Item } from '$lib/types';
import { childState, countChildProgress, countedChildren } from './childProgress';

const coll = (slug: string, fields: Record<string, unknown>[], settings: Record<string, unknown> = {}) =>
	({ slug, schema: JSON.stringify({ fields }), settings: JSON.stringify(settings) }) as unknown as Collection;
const child = (id: string, collection_slug: string, fields: Record<string, unknown>) =>
	({ id, collection_slug, fields: JSON.stringify(fields) }) as unknown as Item;

const tasks = coll('tasks', [
	{ key: 'status', type: 'select', terminal_options: ['done', 'cancelled'], abandoned_options: ['cancelled'] }
]);

describe('countChildProgress (BUG-3195)', () => {
	it('leaves an abandoned child out of both done and total', () => {
		const kids = [child('a', 'tasks', { status: 'done' }), child('b', 'tasks', { status: 'cancelled' }), child('c', 'tasks', { status: 'open' })];
		expect(countChildProgress(kids, [tasks])).toEqual({ done: 1, total: 2 });
	});

	it('gives 0/0 when every child is abandoned', () => {
		expect(countChildProgress([child('a', 'tasks', { status: 'cancelled' })], [tasks])).toEqual({ done: 0, total: 0 });
	});

	it('judges each child by its own collection, not a merged list', () => {
		// "cancelled" is abandoned in tasks but a delivered terminal in legacy,
		// which declares its own abandoned list without it.
		const legacy = coll('legacy', [
			{ key: 'status', type: 'select', terminal_options: ['cancelled', 'dropped'], abandoned_options: ['dropped'] }
		]);
		const kids = [child('a', 'tasks', { status: 'cancelled' }), child('b', 'legacy', { status: 'cancelled' })];
		expect(countChildProgress(kids, [tasks, legacy])).toEqual({ done: 1, total: 1 });
	});

	it('reads the collection done field when board_group_by names a select field', () => {
		const bugs = coll(
			'bugs',
			[
				{ key: 'status', type: 'select', terminal_options: ['fixed'] },
				{ key: 'resolution', type: 'select', terminal_options: ['fixed', 'wontfix'] }
			],
			{ board_group_by: 'resolution' }
		);
		const kids = [
			child('a', 'bugs', { resolution: 'fixed', status: 'new' }),
			child('b', 'bugs', { resolution: 'wontfix', status: 'new' }),
			child('c', 'bugs', { resolution: 'open', status: 'fixed' })
		];
		// wontfix is abandoned (fallback), c's terminal `status` is not the done field.
		expect(countChildProgress(kids, [bugs])).toEqual({ done: 1, total: 2 });
	});

	it('compares case-insensitively, as the server does', () => {
		const kids = [child('a', 'tasks', { status: 'Done' }), child('b', 'tasks', { status: 'CANCELLED' })];
		expect(countChildProgress(kids, [tasks])).toEqual({ done: 1, total: 1 });
	});

	it('control: with nothing abandoned the same child counts as done', () => {
		const plain = coll('plain', [{ key: 'status', type: 'select', terminal_options: ['done', 'shipped'] }]);
		expect(countChildProgress([child('a', 'plain', { status: 'done' }), child('b', 'plain', { status: 'shipped' })], [plain])).toEqual({
			done: 2,
			total: 2
		});
	});

	it('falls back to the default lists when the collection is unknown', () => {
		expect(childState(child('a', 'gone', { status: 'wontfix' }), undefined)).toBe('out');
		expect(childState(child('b', 'gone', { status: 'done' }), undefined)).toBe('done');
		expect(childState(child('c', 'gone', { status: 'open' }), undefined)).toBe('open');
	});

	it('countedChildren keeps every non-abandoned child in order', () => {
		const kids = [child('open', 'tasks', { status: 'open' }), child('x', 'tasks', { status: 'cancelled' }), child('done', 'tasks', { status: 'done' })];
		expect(countedChildren(kids, [tasks]).map((c) => c.id)).toEqual(['open', 'done']);
	});
});

describe('getAbandonedOptions (BUG-3195)', () => {
	it('uses the done field’s own abandoned_options when declared', () => {
		expect(getAbandonedOptions(coll('x', [{ key: 'status', type: 'select', terminal_options: ['done', 'overturned'], abandoned_options: ['overturned'] }]))).toEqual(['overturned']);
	});

	it('falls back to the terminal options that are negative outcomes', () => {
		expect(getAbandonedOptions(coll('x', [{ key: 'status', type: 'select', terminal_options: ['fixed', 'wontfix', 'duplicate'] }]))).toEqual(['wontfix', 'duplicate']);
	});

	it('with no terminal options, applies the fallback to the default terminal list', () => {
		expect(getAbandonedOptions(coll('x', [{ key: 'status', type: 'select' }]))).toEqual(DEFAULT_ABANDONED_STATUSES);
		expect(DEFAULT_ABANDONED_STATUSES).toEqual(['cancelled', 'rejected', 'wontfix', 'disabled']);
	});
});
