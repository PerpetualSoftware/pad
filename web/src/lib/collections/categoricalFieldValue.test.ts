// BUG-3067 — the pure core of "may this surface print this field as a
// categorical value?", tested without a store because it takes the collections.
//
// The class: a `status` or `priority` field retyped to a `relation` stores a
// STRING (an item id), so a surface that reads the value by name and prints it
// shows a raw uuid. BUG-3016 fixed the sites that had a `FieldDef` in hand;
// these are the ones that read by NAME with no schema.
import { describe, it, expect } from 'vitest';
import type { Collection } from '$lib/types';
import {
	categoricalValueFor,
	categoricalValueForSlug,
	collectionsNotStaleFor,
	fieldDefFor,
} from './categoricalFieldValue';

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

describe('a slug is only unique within a workspace', () => {
	// THE CROSS-WORKSPACE HAZARD, found by the review round. `collectionStore`
	// keeps ONE global array and deliberately retains the previous workspace's
	// collections while the next load is in flight (the store documents this,
	// from BUG-1461). Two workspaces can both have a `tasks` whose `priority` is
	// a select in one and a relation in the other, so during that window a
	// slug-only lookup can answer with the wrong workspace's TYPE — in either
	// direction, printing an id or withholding a good value.
	const item = (slug: string) => ({ collection_slug: slug }) as never;

	it('answers nothing when the loaded collections are not this workspace\'s', () => {
		expect(categoricalValueFor(ALL, item('tasks'), 'status', 'open', false)).toBe('');
		expect(fieldDefFor(ALL, 'tasks', 'status', false)).toBeUndefined();
	});

	it('CONTROL: the same call answers normally when they are', () => {
		// Without this, a helper hard-wired to return '' would pass the leg above
		// and blank every chip in the app.
		expect(categoricalValueFor(ALL, item('tasks'), 'status', 'open', true)).toBe('open');
	});

	it('defaults to fresh, so a caller that cannot answer the question is not silently blanked', () => {
		// The default is deliberate and worth pinning: a surface with no workspace
		// slug in hand still works, and the stale window is the caller's to close
		// where it can. Changing this default would blank every call site that
		// omits the argument.
		expect(categoricalValueFor(ALL, item('tasks'), 'status', 'open')).toBe('open');
	});
});

describe('categoricalValueForSlug', () => {
	// The entry point for a surface holding a server projection rather than an
	// Item — the graph node. Same question, same module; it existed as a direct
	// `categoricalChipValue` call until the review round pointed out that made
	// "every surface routes through one helper" false by exactly one call.
	it('answers identically to the item form for the same collection', () => {
		expect(categoricalValueForSlug(ALL, 'tasks', 'status', 'open')).toBe('open');
		expect(categoricalValueForSlug(ALL, 'cars', 'status', ID)).toBe('');
	});
});

describe('collectionsNotStaleFor — the narrower question', () => {
	// THE REGRESSION THIS REPLACED, pinned so it cannot come back. The first
	// version asked the store's `collectionsAreFreshFor`, which is false in TWO
	// situations: the array belongs to another workspace, and no load has been
	// stamped at all. Only the first is a reason to withhold a value.
	//
	// The second is routine — `playbooks` and the dashboard fetch collections
	// into PAGE-LOCAL state and never stamp the shared store, so the stamp comes
	// from the workspace layout. Treating unstamped as stale blanked every status
	// pill on those pages until the layout's load landed, and permanently if it
	// failed while their own fetch succeeded.
	it('is TRUE when nothing has been stamped yet — the case that caused the blanking', () => {
		expect(collectionsNotStaleFor(null, 'ws-a')).toBe(true);
	});

	it('is FALSE only when the stamp names a different workspace', () => {
		expect(collectionsNotStaleFor('ws-b', 'ws-a')).toBe(false);
	});

	it('is TRUE when the stamp matches', () => {
		expect(collectionsNotStaleFor('ws-a', 'ws-a')).toBe(true);
	});

	it('is TRUE when the caller has no workspace slug to compare', () => {
		// A surface with no workspace in hand must not be blanked; it falls back
		// to the lookup, which answers undefined for an unloaded array anyway.
		expect(collectionsNotStaleFor('ws-b', undefined)).toBe(true);
	});

	it('CONTROL: an unstamped store still yields nothing when the array is empty', () => {
		// The unloaded case was always covered by the DATA, never by this flag —
		// which is the argument for narrowing the flag rather than widening it.
		expect(categoricalValueFor([], { collection_slug: 'tasks' } as never, 'status', 'open')).toBe('');
	});
});

describe('collectionsNotStaleFor treats both empty spellings alike', () => {
	// The hole a control leg caught the moment the DetailCard test started
	// passing a real `wsSlug`: the first version compared against `null` only, so
	// an `undefined` stamp fell through to the equality and answered STALE for
	// every workspace — blanking every chip on a surface whose store mock, or
	// future store, happened to spell "nothing loaded" the other way.
	it('undefined is not stale', () => {
		expect(collectionsNotStaleFor(undefined, 'ws-a')).toBe(true);
	});

	it('null is not stale', () => {
		expect(collectionsNotStaleFor(null, 'ws-a')).toBe(true);
	});

	it('CONTROL: a real mismatch is still stale', () => {
		// Without this, a guard that returned true unconditionally would satisfy
		// both legs above and the staleness check would be decoration.
		expect(collectionsNotStaleFor('ws-b', 'ws-a')).toBe(false);
	});
});
