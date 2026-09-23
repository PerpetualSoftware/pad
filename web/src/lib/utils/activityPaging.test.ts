import { describe, expect, it } from 'vitest';
import { appendUnique, cursorAfter } from './activityPaging';

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
