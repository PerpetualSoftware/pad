import { describe, it, expect } from 'vitest';
import { LINK_DIRECTIONS, linkDirection, linkEnds, linkSentence } from './linkDirections';

const current = { slug: 'task-5', id: 'id-5' };
const picked = { slug: 'task-8', id: 'id-8' };

describe('Add Relationship directions (TASK-2217)', () => {
	it('every inverse is the same type with the ends swapped', () => {
		expect(linkEnds(linkDirection('blocks'), current, picked)).toEqual({ fromSlug: 'task-5', targetId: 'id-8' });
		expect(linkEnds(linkDirection('blocked_by'), current, picked)).toEqual({ fromSlug: 'task-8', targetId: 'id-5' });
		expect(linkDirection('blocked_by').type).toBe('blocks');
		expect(linkDirection('parent_of')).toMatchObject({ type: 'parent', inverse: true });
	});

	it('offers an inverse for every directed type, and none for related', () => {
		const types = new Set(LINK_DIRECTIONS.map((d) => d.type));
		for (const t of types) {
			const forward = LINK_DIRECTIONS.some((d) => d.type === t && !d.inverse);
			const inverse = LINK_DIRECTIONS.some((d) => d.type === t && d.inverse);
			expect(forward, t).toBe(true);
			expect(inverse, t).toBe(t !== 'related');
		}
	});

	it('says which way each option points', () => {
		expect(linkSentence(linkDirection('blocks'), 'TASK-5')).toBe('TASK-5 blocks the item you pick');
		expect(linkSentence(linkDirection('blocked_by'), 'TASK-5')).toBe('the item you pick blocks TASK-5');
		// parent links are stored child → parent: the existing option makes THIS the child.
		expect(linkSentence(linkDirection('child_of'), 'TASK-5')).toBe('the item you pick becomes the parent of TASK-5');
	});

	it('values are unique and an unknown value falls back to related', () => {
		expect(new Set(LINK_DIRECTIONS.map((d) => d.value)).size).toBe(LINK_DIRECTIONS.length);
		expect(linkDirection('bogus').value).toBe('related');
	});
});
