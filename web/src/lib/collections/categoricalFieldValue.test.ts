// BUG-3067 — the pure core of "may this surface print this field as a
// categorical value?", tested without a store because it takes the collections.
//
// The class: a `status` or `priority` field retyped to a `relation` stores a
// STRING (an item id), so a surface that reads the value by name and prints it
// shows a raw uuid. BUG-3016 fixed the sites that had a `FieldDef` in hand;
// these are the ones that read by NAME with no schema.
import { describe, it, expect } from 'vitest';
import type { Collection } from '$lib/types';
import { categoricalValueFor, fieldDefFor } from './categoricalFieldValue';

function collection(slug: string, fields: unknown[]): Collection {
	return {
		id: `c-${slug}`,
		workspace_id: 'ws1',
		name: slug,
		slug,
		icon: '',
		description: '',
		schema: JSON.stringify({ fields }),
		settings: '{}',
		sort_order: 0,
		is_default: false,
		is_system: false,
		created_at: 'x',
		updated_at: 'x',
		prefix: slug.toUpperCase(),
	} as unknown as Collection;
}

const TASKS = collection('tasks', [
	{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
	{ key: 'priority', label: 'Priority', type: 'select', options: ['low', 'high'] },
]);
// The same two KEYS, retyped — which is the whole defect: a key is not a type.
const RETYPED = collection('cars', [
	{ key: 'status', label: 'Status', type: 'relation', collection: 'colors' },
	{ key: 'priority', label: 'Priority', type: 'multi_relation', collection: 'colors' },
]);
const ALL = [TASKS, RETYPED];

const ID = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';

describe('fieldDefFor', () => {
	it('finds the declared field in the named collection', () => {
		expect(fieldDefFor(ALL, 'tasks', 'status')?.type).toBe('select');
		expect(fieldDefFor(ALL, 'cars', 'status')?.type).toBe('relation');
	});

	it('answers undefined for an unknown collection, an unknown field, or no slug', () => {
		expect(fieldDefFor(ALL, 'nope', 'status')).toBeUndefined();
		expect(fieldDefFor(ALL, 'tasks', 'nope')).toBeUndefined();
		expect(fieldDefFor(ALL, undefined, 'status')).toBeUndefined();
	});

	it('resolves per COLLECTION, not per key — two collections, same key, different types', () => {
		// The assertion the whole class rests on. A surface that renders rows from
		// several collections cannot cache one answer for a key.
		expect(fieldDefFor(ALL, 'tasks', 'priority')?.type).toBe('select');
		expect(fieldDefFor(ALL, 'cars', 'priority')?.type).toBe('multi_relation');
	});
});

describe('categoricalValueFor', () => {
	const item = (slug: string) => ({ collection_slug: slug }) as never;

	it('returns the value for an ordinary select', () => {
		expect(categoricalValueFor(ALL, item('tasks'), 'status', 'open')).toBe('open');
		expect(categoricalValueFor(ALL, item('tasks'), 'priority', 'high')).toBe('high');
	});

	it('WITHHOLDS a scalar relation value, which is an id and not a status', () => {
		// The defect itself: this value IS a string, so every "is it a string"
		// test passes it through. Only the declared type can refuse it.
		expect(categoricalValueFor(ALL, item('cars'), 'status', ID)).toBe('');
	});

	it('withholds a list value too', () => {
		expect(categoricalValueFor(ALL, item('cars'), 'priority', [ID])).toBe('');
	});

	it('withholds when the collection is not loaded yet — fail closed', () => {
		// Documented in the helper and asserted here because it is the one empty
		// answer that is NOT an error: during store load a row renders without its
		// chip rather than printing a value nothing has vouched for. A caller must
		// not read '' as "this item has no status".
		expect(categoricalValueFor([], item('tasks'), 'status', 'open')).toBe('');
	});

	it('withholds a non-string value on an ordinary field', () => {
		expect(categoricalValueFor(ALL, item('tasks'), 'status', 42)).toBe('');
		expect(categoricalValueFor(ALL, item('tasks'), 'status', null)).toBe('');
	});

	it('CONTROL: withholding is about the TYPE, not about the value looking like an id', () => {
		// Without this, a guard that simply refused uuid-shaped strings would pass
		// every leg above — and would then also refuse a legitimately uuid-named
		// status option, while still printing an id stored in a relation field
		// whose target happened to have a short id.
		expect(categoricalValueFor(ALL, item('tasks'), 'status', ID)).toBe(ID);
	});
});
