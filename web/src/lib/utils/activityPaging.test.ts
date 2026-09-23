import { describe, expect, it } from 'vitest';
import { appendUnique, cursorAfter, mergeHead } from './activityPaging';

describe('cursorAfter', () => {
	it('names the LAST held row', () => {
		expect(
			cursorAfter([
				{ id: 'a', created_at: '2026-01-01T00:00:02Z' },
				{ id: 'b', created_at: '2026-01-01T00:00:01Z' }
			])
		).toEqual({ before: '2026-01-01T00:00:01Z', before_id: 'b' });
	});

	it('is null for no rows, so the first page carries no cursor', () => {
		expect(cursorAfter([])).toBeNull();
	});
});

describe('appendUnique', () => {
	it('drops a repeated id and keeps held order', () => {
		const held = [{ id: 'a' }, { id: 'b' }];
		expect(appendUnique(held, [{ id: 'b' }, { id: 'c' }]).map((r) => r.id)).toEqual(['a', 'b', 'c']);
	});

	it('drops a repeat inside one page too', () => {
		expect(appendUnique([], [{ id: 'x' }, { id: 'x' }]).map((r) => r.id)).toEqual(['x']);
	});

	it('does not mutate what it was given', () => {
		const held = [{ id: 'a' }];
		appendUnique(held, [{ id: 'b' }]);
		expect(held).toEqual([{ id: 'a' }]);
	});
});

describe('mergeHead (BUG-3160)', () => {
	const row = (id: string, created_at: string, note = '') => ({ id, created_at, note });

	it('a row the debounce merge restamped moves to the top with its new content', () => {
		const held = [row('b', '2026-01-01T00:00:02Z'), row('a', '2026-01-01T00:00:01Z', 'old')];
		const fresh = [row('a', '2026-01-01T00:00:09Z', 'merged'), row('b', '2026-01-01T00:00:02Z')];
		const out = mergeHead(held, fresh);
		expect(out.map((r) => r.id)).toEqual(['a', 'b']);
		expect(out[0].note).toBe('merged');
	});

	it('keeps a held row absent from the fresh head — rolled off is not deleted', () => {
		const held = [row('c', '2026-01-01T00:00:03Z'), row('old', '2020-01-01T00:00:00Z')];
		const fresh = [row('c', '2026-01-01T00:00:03Z')];
		expect(mergeHead(held, fresh).map((r) => r.id)).toEqual(['c', 'old']);
	});

	it('adds new rows and never duplicates an id', () => {
		const held = [row('a', '2026-01-01T00:00:01Z')];
		const fresh = [row('n', '2026-01-01T00:00:05Z'), row('a', '2026-01-01T00:00:01Z')];
		const out = mergeHead(held, fresh);
		expect(out.map((r) => r.id)).toEqual(['n', 'a']);
	});

	it('breaks created_at ties by id descending, the server feed order', () => {
		const t = '2026-01-01T00:00:01Z';
		expect(mergeHead([row('a', t)], [row('c', t), row('b', t)]).map((r) => r.id)).toEqual(['c', 'b', 'a']);
	});
});
