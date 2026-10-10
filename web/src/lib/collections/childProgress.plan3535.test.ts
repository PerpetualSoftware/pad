import { describe, expect, it } from 'vitest';
import type { Collection, Item } from '$lib/types';
import {
	countChildProgress,
	countedChildren,
	isReferenceChild,
	splitReferenceChildren,
	statusState
} from './childProgress';

// PLAN-3535: a child of a REFERENCE collection (tracks_work: false) is not part
// of its parent's work. It leaves both progress numbers whatever its status,
// and the parent page lists it apart.

const coll = (slug: string, settings: Record<string, unknown> = {}) =>
	({
		slug,
		schema: JSON.stringify({ fields: [{ key: 'status', type: 'select', options: ['draft', 'open', 'done', 'cancelled'], terminal_options: ['done', 'cancelled'], abandoned_options: ['cancelled'] }] }),
		settings: JSON.stringify(settings)
	}) as unknown as Collection;
const child = (id: string, collection_slug: string, status: string) =>
	({ id, collection_slug, fields: JSON.stringify({ status }) }) as unknown as Item;

const tasks = coll('tasks');
const docs = coll('docs', { tracks_work: false });
const kids = [child('t1', 'tasks', 'done'), child('t2', 'tasks', 'open'), child('d1', 'docs', 'draft'), child('d2', 'docs', 'done')];

describe('reference children (PLAN-3535)', () => {
	it('leaves them out of both progress numbers, whatever their status', () => {
		expect(countChildProgress(kids, [tasks, docs])).toEqual({ done: 1, total: 2 });
		expect(countedChildren(kids, [tasks, docs]).map((c) => c.id)).toEqual(['t1', 't2']);
	});

	it('counts them as work when the collection tracks work (absent key) or is unknown', () => {
		const asWork = coll('docs');
		expect(countChildProgress(kids, [tasks, asWork])).toEqual({ done: 2, total: 4 });
		expect(countChildProgress(kids, [tasks])).toEqual({ done: 2, total: 4 });
		expect(isReferenceChild(kids[2], [])).toBe(false);
	});

	it('splits them from the work children, keeping order', () => {
		const { work, reference } = splitReferenceChildren(kids, [tasks, docs]);
		expect(work.map((c) => c.id)).toEqual(['t1', 't2']);
		expect(reference.map((c) => c.id)).toEqual(['d1', 'd2']);
	});
});

describe('statusState', () => {
	it("judges a status by the collection's own done rule", () => {
		expect(statusState(tasks, 'done')).toBe('done');
		expect(statusState(tasks, 'cancelled')).toBe('out');
		expect(statusState(tasks, 'open')).toBe('open');
	});

	it('falls back to the default lists when the done field is not status', () => {
		const staged = {
			slug: 'staged',
			schema: JSON.stringify({ fields: [{ key: 'stage', type: 'select', options: ['a', 'z'], terminal_options: ['z'] }] }),
			settings: JSON.stringify({ board_group_by: 'stage' })
		} as unknown as Collection;
		expect(statusState(staged, 'done')).toBe('done');
		expect(statusState(staged, 'z')).toBe('open');
	});
});
