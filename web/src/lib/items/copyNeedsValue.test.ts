import { describe, it, expect } from 'vitest';
import { isCollectable, uncollectableReason, COLLECTABLE_TYPES } from './copyNeedsValue';
import type { ItemCopyPreflightNeedsValue } from '$lib/types';

const row = (over: Partial<ItemCopyPreflightNeedsValue>): ItemCopyPreflightNeedsValue => ({
	key: 'owner_ref',
	required: true,
	reason: 'missing_required',
	...over,
});

describe('isCollectable', () => {
	it('accepts a relation that names its target collection', () => {
		expect(isCollectable(row({ type: 'relation', collection: 'people-b' }))).toBe(true);
	});

	// THE NEGATIVE LEG of TASK-2869's proving test, and the reason this module
	// exists. FieldEditor gates its relation branch on wsSlug AND
	// field.collection, so a relation row with no collection cannot be filled
	// in — offering it would render a dead control, or an unscoped picker
	// listing SOURCE-workspace items the copy cannot point at.
	it('refuses a relation with no target collection', () => {
		expect(isCollectable(row({ type: 'relation' }))).toBe(false);
		expect(isCollectable(row({ type: 'relation', collection: '' }))).toBe(false);
	});

	it('is unaffected for every other type', () => {
		for (const type of COLLECTABLE_TYPES) {
			expect(isCollectable(row({ type })), `${type} should stay collectable`).toBe(true);
		}
		// The two FieldEditor cannot safely collect, named individually rather
		// than as "everything else" so a new dangerous type does not join them
		// silently.
		expect(isCollectable(row({ type: 'multi_select' }))).toBe(false);
		expect(isCollectable(row({ type: 'json' }))).toBe(false);
	});

	it('treats a missing type as text, which is collectable', () => {
		expect(isCollectable(row({}))).toBe(true);
	});
});

describe('IDEA-2899 — a relation whose target is not usable', () => {
	it('refuses a relation whose target the server reports unavailable', () => {
		expect(
			isCollectable(row({ type: 'relation', collection: 'people-b', collection_unavailable: true }))
		).toBe(false);
	});

	// THE CONTRACT, and the reason the flag is phrased negatively. `omitempty`
	// on a Go bool drops FALSE, so the server omits the field when the target is
	// fine — and a server predating this change omits it always. Absence is "no
	// information" and must never block; a client that read it as a value would
	// refuse every relation row against an older server.
	it('treats an ABSENT flag as no information, not as unavailable', () => {
		expect(isCollectable(row({ type: 'relation', collection: 'people-b' }))).toBe(true);
		expect(
			isCollectable(row({ type: 'relation', collection: 'people-b', collection_unavailable: false }))
		).toBe(true);
	});

	// `undefined` stated explicitly, since it is the shape an older server
	// produces and the one the whole contract turns on.
	//
	// NOT a test of the strict `=== true` spelling: over the domain this
	// field's type admits (`boolean | undefined`) the truthiness form is
	// equivalent, a mutant swapping it in survives, and manufacturing an
	// off-contract value to kill that mutant would be testing a rule nobody
	// holds. The reason for the strict form is recorded in the source.
	it('treats an explicitly undefined flag as no information', () => {
		const odd = { type: 'relation', collection: 'people-b', collection_unavailable: undefined };
		expect(isCollectable(row(odd as Partial<ItemCopyPreflightNeedsValue>))).toBe(true);
	});

	it('does not touch non-relation rows carrying the flag', () => {
		// The server gates the flag on the field TYPE, so this shape should not
		// arrive. Asserted anyway: the client must not grow a second, laxer
		// definition of what the flag means, because two definitions of one rule
		// is how the pair drifts.
		expect(isCollectable(row({ type: 'text', collection_unavailable: true }))).toBe(true);
		expect(isCollectable(row({ type: 'select', collection_unavailable: true }))).toBe(true);
	});
});

describe('uncollectableReason', () => {
	it('is null for anything collectable', () => {
		expect(uncollectableReason(row({ type: 'text' }))).toBeNull();
		expect(uncollectableReason(row({ type: 'relation', collection: 'people-b' }))).toBeNull();
	});

	// The distinction that earns this function: the dialog offers a CLI command
	// for type-shaped failures and must NOT offer one here. The CLI runs as the
	// same user against the same referent validation, so the printed command
	// would be refused for the same reason.
	it('separates an unavailable target from a type it cannot collect', () => {
		expect(
			uncollectableReason(
				row({ type: 'relation', collection: 'people-b', collection_unavailable: true })
			)
		).toBe('unavailable_target');
		expect(uncollectableReason(row({ type: 'json' }))).toBe('type');
		expect(uncollectableReason(row({ type: 'multi_select' }))).toBe('type');
	});

	// A relation naming NO target is TASK-2869's case and stays type-shaped:
	// there is no collection to name in the message, so the copy that names one
	// would render an empty code span.
	it('calls a relation with no target a type failure, not an unavailable one', () => {
		expect(uncollectableReason(row({ type: 'relation' }))).toBe('type');
	});
});
