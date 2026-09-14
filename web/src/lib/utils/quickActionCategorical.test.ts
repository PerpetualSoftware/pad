// BUG-3067, lead ruling on the trail — the prompt-substitution half of the class.
//
// The chips answer a retyped `status`/`priority` by WITHHOLDING the value. A
// prompt variable cannot: `{status}` has to become something, and a blank is a
// lie of a different kind. The ruling matches what #1352 did for chips —
// resolve to the target's TITLE, fall back to its REF, never emit the raw id.
// The FALLBACK IS FOR A RESOLVED ROW WITH NO TITLE — an earlier version of this
// comment said "when the row cannot be resolved", which the code cannot do and
// the legs below show it does not: a ref is built from the row, so an id that
// resolves to nothing has no ref to fall back to and yields empty.
//
// The second half of the ruling is structural: `QuickActionsMenu` and
// `quick-action-preview` held byte-identical copies of this substitution, and
// the preview module's own doc says it MIRRORS the menu so the preview shows
// what copying produces. Two mirrors drift, and a preview whose purpose is to
// match makes the drift invisible. They are one function now, imported.
import { describe, it, expect } from 'vitest';
import type { Collection, ItemIndexRow } from '$lib/types';
import { categoricalTemplateValue } from './quick-action-preview';

function collection(fields: unknown[]): Collection {
	return {
		id: 'c1', workspace_id: 'ws1', name: 'Cars', slug: 'cars', icon: '', description: '',
		schema: JSON.stringify({ fields }), settings: '{}', sort_order: 0,
		is_default: false, is_system: false, created_at: 'x', updated_at: 'x', prefix: 'CAR',
	} as unknown as Collection;
}

const ORDINARY = collection([{ key: 'status', label: 'Status', type: 'select', options: ['open'] }]);
const RETYPED = collection([{ key: 'status', label: 'Status', type: 'relation', collection: 'colors' }]);
const LIST = collection([{ key: 'status', label: 'Status', type: 'multi_relation', collection: 'colors' }]);

const RED = 'id-red';
const BLUE = 'id-blue';
const NAMELESS = 'id-nameless';
const GONE = 'id-gone';

const ROWS: Record<string, ItemIndexRow> = {
	[RED]: { id: RED, title: 'Red', item_number: 1, collection_prefix: 'COLOR', collection_slug: 'colors', deleted_at: null } as unknown as ItemIndexRow,
	[BLUE]: { id: BLUE, title: 'Blue', item_number: 2, collection_prefix: 'COLOR', collection_slug: 'colors', deleted_at: null } as unknown as ItemIndexRow,
	// Title-less on purpose: this is what makes the REF fallback reachable.
	[NAMELESS]: { id: NAMELESS, title: '', item_number: 7, collection_prefix: 'COLOR', collection_slug: 'colors', deleted_at: null } as unknown as ItemIndexRow,
};
const resolve = (id: string) => ROWS[id] ?? null;

describe('a categorical template variable', () => {
	it('passes an ordinary value straight through', () => {
		expect(categoricalTemplateValue(ORDINARY, 'status', 'open', resolve)).toBe('open');
	});

	it('resolves a relation to the TITLE, never the id', () => {
		const out = categoricalTemplateValue(RETYPED, 'status', RED, resolve);
		expect(out).toBe('Red');
		expect(out).not.toContain(RED);
	});

	it('falls back to the REF when the row has no title', () => {
		// The fallback the ruling names, and it needs a title-less row to be
		// reachable at all — a fixture that always has a title would make this
		// leg pass without ever exercising the branch.
		expect(categoricalTemplateValue(RETYPED, 'status', NAMELESS, resolve)).toBe('COLOR-7');
	});

	it('joins a list with a comma and a space', () => {
		expect(categoricalTemplateValue(LIST, 'status', [RED, BLUE], resolve)).toBe('Red, Blue');
	});

	it('emits nothing for an id that resolves to nothing — and never the id itself', () => {
		// Empty here is not a choice, it is what is left when there is nothing
		// true to say. The assertion that matters is the second one.
		const out = categoricalTemplateValue(RETYPED, 'status', GONE, resolve);
		expect(out).toBe('');
		expect(out).not.toContain(GONE);
	});

	it('drops only the unresolvable element of a list', () => {
		expect(categoricalTemplateValue(LIST, 'status', [RED, GONE, BLUE], resolve)).toBe('Red, Blue');
	});

	it('CONTROL: with no collection in hand it still does not invent a lookup', () => {
		// A caller that cannot supply the schema gets the old stringify. That is
		// the honest degradation: this helper cannot tell a relation from a
		// select without the declared type, and guessing from the value's shape
		// is the mistake the whole class is made of.
		expect(categoricalTemplateValue(undefined, 'status', 'open', resolve)).toBe('open');
	});
});
