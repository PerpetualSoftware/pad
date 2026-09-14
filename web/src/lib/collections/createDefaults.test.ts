// Node-project test: the status default a create starts with (BUG-3078).
//
// The four arms below are the four outcomes MEASURED against the real create
// handler before this module was written, not four cases imagined from the
// types. On a collection whose `status` is `multi_select` with a stale scalar
// default that survived the retype:
//
//   supplied `{"status":"open"}`   -> 400, "must be an array of strings"
//   omitted  `{}`                  -> 201, stored {"status":"open"}  (unchecked)
//   converted `{"status":["open"]}` -> 201, stored {"status":["open"]}
//   omitted, no stale default      -> 201, stored {}
//
// That is why this converts where it can and omits only where it cannot: the
// converted write is the one arm that both lands AND stores a correctly-typed
// value. The probe and its numbers are on the BUG-3078 trail.
import { describe, it, expect } from 'vitest';
import { createDefaultFields } from './createDefaults';
import { laneWriteValue } from './laneWriteValue';
import type { Collection } from '$lib/types';

const coll = (statusField: Record<string, unknown> | null): Collection =>
	({
		slug: 'tasks',
		name: 'Tasks',
		schema: JSON.stringify({ fields: statusField ? [statusField] : [] }),
		settings: '{}',
	}) as unknown as Collection;

describe('createDefaultFields', () => {
	it('uses the first option for a select — the case that was never broken', () => {
		expect(createDefaultFields(coll({ key: 'status', type: 'select', options: ['open', 'done'] })))
			.toEqual({ status: 'open' });
	});

	it('CONVERTS for a multi_select instead of omitting', () => {
		// The arm the server accepts and stores correctly typed. Omitting here
		// would fall through to the schema default, which is assigned WITHOUT a
		// type check — so omitting would be strictly worse when that default is
		// a stale scalar.
		expect(
			createDefaultFields(coll({ key: 'status', type: 'multi_select', options: ['open', 'done'] })),
		).toEqual({ status: ['open'] });
	});

	it('OMITS for number and checkbox, where no conversion exists', () => {
		// Reachable only because `options` survives a retype; a field that was
		// always a number has none and never reached the old code either.
		//
		// KEY ABSENCE, not `toEqual({})`. A mutant that drops the `ok` check
		// assigns `undefined` — `laneWriteValue`'s refusal carries no `value` —
		// and `toEqual` treats an undefined-valued key as absent, so it SURVIVED
		// the first version of this test. `Object.keys` is what tells the two
		// apart. It matters beyond the mutant only for a future caller that
		// spreads or enumerates the result: today's callers `JSON.stringify` it,
		// which drops an undefined value, so the wire payload is identical and
		// the distinction is the module's contract rather than an observed bug.
		for (const type of ['number', 'checkbox']) {
			const got = createDefaultFields(coll({ key: 'status', type, options: ['open', 'done'] }));
			expect(Object.keys(got), type).toEqual([]);
			expect('status' in got, type).toBe(false);
		}
	});

	it('OMITS for relation and multi_relation, whose values are item ids', () => {
		// Codex round 1 found `multi_relation`; `relation` is the same class and
		// came from asking what else reaches that branch. Both refusals were
		// measured against the create handler, not reasoned about:
		//   relation       -> 400 "open" does not name an item in collection "tasks"
		//   multi_relation -> 400 must be an array of strings (item IDs, refs, …)
		// There is no honest conversion for either: wrapping as ['open'] clears
		// the shape check and is then refused by referent validation, because a
		// leftover select option names no item.
		for (const type of ['relation', 'multi_relation']) {
			const got = createDefaultFields(
				coll({ key: 'status', type, collection: 'tasks', options: ['open', 'done'] }),
			);
			expect(Object.keys(got), type).toEqual([]);
		}
	});

	it('does NOT let laneWriteValue decide the reference case, because it cannot', () => {
		// Pins the asymmetry rather than the outcome, so a later "simplification"
		// that deletes the REFERENCE_TYPES guard and leans on laneWriteValue
		// fails here with the reason attached. `laneWriteValue` passes a relation
		// key straight through BY DESIGN — a lane key for a relation is already
		// the target's item id. `options[0]` is not a lane key and never an id.
		const relField = { key: 'status', type: 'relation', collection: 'tasks', options: ['open'] };
		expect(laneWriteValue(relField as never, 'open')).toEqual({ ok: true, value: 'open' });
		expect(Object.keys(createDefaultFields(coll(relField)))).toEqual([]);
	});

	it('omits when the field declares no options at all', () => {
		expect(Object.keys(createDefaultFields(coll({ key: 'status', type: 'select' })))).toEqual([]);
		expect(
			Object.keys(createDefaultFields(coll({ key: 'status', type: 'select', options: [] }))),
		).toEqual([]);
	});

	it('omits when there is no status field, and when there is no collection', () => {
		expect(createDefaultFields(coll(null))).toEqual({});
		expect(createDefaultFields(coll({ key: 'priority', type: 'select', options: ['high'] }))).toEqual({});
		expect(createDefaultFields(null)).toEqual({});
		expect(createDefaultFields(undefined)).toEqual({});
	});

	it('survives a malformed schema rather than throwing', () => {
		// `parseSchema` swallows the parse error and returns defaults; a create
		// path that threw here would be a worse failure than the one being fixed.
		expect(createDefaultFields({ schema: 'not json', settings: '{}' } as unknown as Collection))
			.toEqual({});
	});

	it('returns a FRESH object each call', () => {
		// Callers mutate the result (they add lane values to it), so a shared
		// object would leak one create's lane value into the next create.
		const c = coll({ key: 'status', type: 'select', options: ['open'] });
		const a = createDefaultFields(c);
		(a as Record<string, unknown>).leaked = true;
		expect(createDefaultFields(c)).toEqual({ status: 'open' });
	});
});
