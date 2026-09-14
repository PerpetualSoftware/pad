// Node-project test: a lane key becomes a value the schema can hold (BUG-3057).
//
// The pairing that matters is with `internal/items`: every value this returns
// has to be one the server's validator ACCEPTS for that type, and every string
// it replaces is one the validator REFUSES. The Go side pins the same table
// from its end (TestLaneKeyWrites_BUG3057), so the two cannot drift into a
// state where this module is "correct" against a validator that has moved.
import { describe, it, expect } from 'vitest';
import { laneWriteValue, laneWriteRefusalMessage, laneKeyIsBulkMovable } from './laneWriteValue';
import type { FieldDef } from '$lib/types';

const f = (type: string, over: Partial<FieldDef> = {}): FieldDef =>
	({ key: 'g', label: 'Group', type, ...over }) as unknown as FieldDef;

describe('laneWriteValue', () => {
	it('passes a string-shaped field its lane key unchanged', () => {
		// select is the common case and was never broken; it is here so a fix
		// that converted everything would be caught.
		expect(laneWriteValue(f('select'), 'done')).toEqual({ ok: true, value: 'done' });
		expect(laneWriteValue(f('text'), 'done')).toEqual({ ok: true, value: 'done' });
		expect(laneWriteValue(f('date'), '2026-01-01')).toEqual({ ok: true, value: '2026-01-01' });
		// The established clear for a select — `''` is a legal write there.
		expect(laneWriteValue(f('select'), '')).toEqual({ ok: true, value: '' });
	});

	it('gives a number field a NUMBER, including zero', () => {
		// `0` is the case BUG-3053 was about on the read side: it is an ordinary
		// value with an ordinary lane, and `'0'` is what the projection makes of
		// it. The write has to undo exactly that.
		expect(laneWriteValue(f('number'), '0')).toEqual({ ok: true, value: 0 });
		expect(laneWriteValue(f('number'), '10')).toEqual({ ok: true, value: 10 });
		expect(laneWriteValue(f('number'), '-2.5')).toEqual({ ok: true, value: -2.5 });
	});

	it('clears a number field with the DELETE sentinel, not an empty string', () => {
		// `''` is what the uncategorised lane means, and the validator refuses it
		// for a number ("must be a number"). `null` is the patch path's delete.
		expect(laneWriteValue(f('number'), '')).toEqual({ ok: true, value: null });
	});

	it('refuses a number lane key that is not a number', () => {
		// Reachable when a field is retyped under a `board_group_by` that still
		// names it: the lanes are still the old options.
		expect(laneWriteValue(f('number'), 'open')).toEqual({ ok: false, reason: 'not_a_number' });
		// Whitespace is not zero. `Number(' ')` is 0, which would turn a blank
		// lane into a real value.
		expect(laneWriteValue(f('number'), '   ')).toEqual({ ok: false, reason: 'not_a_number' });
		expect(laneWriteValue(f('number'), 'NaN')).toEqual({ ok: false, reason: 'not_a_number' });
		expect(laneWriteValue(f('number'), 'Infinity')).toEqual({ ok: false, reason: 'not_a_number' });
	});

	it('gives a checkbox field a BOOLEAN, and only for the two spellings the projection makes', () => {
		expect(laneWriteValue(f('checkbox'), 'true')).toEqual({ ok: true, value: true });
		expect(laneWriteValue(f('checkbox'), 'false')).toEqual({ ok: true, value: false });
		expect(laneWriteValue(f('checkbox'), '')).toEqual({ ok: true, value: null });
		// Not guesses: nothing in this seam was typed by a user, so 'yes' / '1' /
		// 'on' would be inventing an intent.
		expect(laneWriteValue(f('checkbox'), '1')).toEqual({ ok: false, reason: 'not_a_boolean' });
		expect(laneWriteValue(f('checkbox'), 'yes')).toEqual({ ok: false, reason: 'not_a_boolean' });
	});

	describe('multi_select — a lane is a COMBINATION, so the write inverts the projection', () => {
		// The picker OFFERS multi_select, so all of this is reachable with no
		// retype. `laneValue(['a','b'])` is `'a,b'` and ListView mints a lane per
		// distinct value, so an `a,b` lane really exists on a grouped list
		// (measured; the board seeds lanes from options, so that item sits in
		// UNCATEGORIZED there instead).
		const tags = f('multi_select', { options: ['a', 'b', 'c'] });

		it('writes BOTH tags for a two-tag lane', () => {
			// The defect this leg exists for: wrapping the key wrote the single
			// tag `"a,b"`, which is not an option and not a tag anyone has.
			expect(laneWriteValue(tags, 'a,b')).toEqual({ ok: true, value: ['a', 'b'] });
		});

		it('CONTROL: a single-tag lane still writes one tag', () => {
			// Without this, "always split on comma" and "always wrap" both pass
			// the leg above for the wrong reason on one side or the other.
			expect(laneWriteValue(tags, 'c')).toEqual({ ok: true, value: ['c'] });
		});

		it('does not TEAR an option that contains a comma', () => {
			// Rule 1 (the key IS an option) before rule 2 (every part is an
			// option). A blind split would write two tags that do not exist.
			const commas = f('multi_select', { options: ['a,b', 'c'] });
			expect(laneWriteValue(commas, 'a,b')).toEqual({ ok: true, value: ['a,b'] });
		});

		it('prefers the REAL option when the lane is genuinely ambiguous', () => {
			// Options `['a,b','a','b']` make the lane `a,b` unreadable in
			// principle — the projection is lossy — so the tie-break is stated
			// rather than left to whichever rule runs first: the reading that
			// names an option that EXISTS wins.
			const ambiguous = f('multi_select', { options: ['a,b', 'a', 'b'] });
			expect(laneWriteValue(ambiguous, 'a,b')).toEqual({ ok: true, value: ['a,b'] });
		});

		it('splits when the field declares NO options', () => {
			// Nothing to check a part against, and splitting is the plain inverse
			// of the join that made the lane.
			expect(laneWriteValue(f('multi_select'), 'a,b')).toEqual({ ok: true, value: ['a', 'b'] });
		});

		it('keeps an unrecognised lane whole', () => {
			// A lane minted from a value that is not in the vocabulary at all:
			// the honest write is the value the lane was named for.
			expect(laneWriteValue(tags, 'legacy,x')).toEqual({ ok: true, value: ['legacy,x'] });
		});

		it('clears with an empty array', () => {
			expect(laneWriteValue(tags, '')).toEqual({ ok: true, value: [] });
		});
	});

	it('writes the key for a field the schema does not declare', () => {
		// Orphan keys are accepted server-side and persist as-is; with no type
		// there is nothing to convert through, and today's behaviour is right.
		expect(laneWriteValue(undefined, 'anything')).toEqual({ ok: true, value: 'anything' });
		expect(laneWriteValue(null, 'anything')).toEqual({ ok: true, value: 'anything' });
	});

	it('names the field in a refusal message', () => {
		expect(laneWriteRefusalMessage('not_a_number', 'Score')).toContain('Score');
		expect(laneWriteRefusalMessage('not_a_boolean', 'Shipped')).toContain('Shipped');
	});
});

describe('laneKeyIsBulkMovable (BUG-3074)', () => {
	// The predicate behind the board lane menu's "Move all to" destinations.
	// The bulk endpoint carries its destination as `Status string`, so the only
	// writes it can express are the ones that resolve to a string.
	const sf = (type: string, options?: string[]) =>
		({ key: 'status', type, options }) as unknown as FieldDef;

	it('allows a select — the type the menu was written for', () => {
		expect(laneKeyIsBulkMovable(sf('select', ['open', 'done']), 'done')).toBe(true);
	});

	it('allows the other string-shaped types', () => {
		for (const t of ['text', 'url', 'date', 'json']) {
			expect(laneKeyIsBulkMovable(sf(t, ['done']), 'done'), t).toBe(true);
		}
	});

	it('allows an undeclared field, matching laneWriteValue', () => {
		expect(laneKeyIsBulkMovable(undefined, 'done')).toBe(true);
	});

	it('refuses multi_select even though laneWriteValue ACCEPTS it', () => {
		// The distinction this predicate exists to draw: `laneWriteValue` is
		// happy to produce `['done']`, and that is a legal write through the
		// per-key patch path. It is the bulk `move` verb's scalar `status` that
		// cannot carry it — so the refusal belongs here, not there.
		const field = sf('multi_select', ['open', 'done']);
		const write = laneWriteValue(field, 'done');
		expect(write.ok).toBe(true);
		expect(Array.isArray((write as { value: unknown }).value)).toBe(true);
		expect(laneKeyIsBulkMovable(field, 'done')).toBe(false);
	});

	it('refuses number and checkbox, which laneWriteValue already refuses', () => {
		expect(laneKeyIsBulkMovable(sf('number', ['open', 'done']), 'done')).toBe(false);
		expect(laneKeyIsBulkMovable(sf('checkbox', ['open', 'done']), 'done')).toBe(false);
	});

	it('refuses a stale option on a retyped field — the reachability the fix turns on', () => {
		// `options` is not stripped by a retype, so this is exactly the input
		// the menu had in hand: a list of real-looking destinations on a field
		// that can no longer hold any of them.
		expect(laneKeyIsBulkMovable(sf('number', ['open', 'done', 'blocked']), 'blocked')).toBe(false);
	});
});
