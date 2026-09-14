// Node-project test: a lane key becomes a value the schema can hold (BUG-3057).
//
// The pairing that matters is with `internal/items`: every value this returns
// has to be one the server's validator ACCEPTS for that type, and every string
// it replaces is one the validator REFUSES. The Go side pins the same table
// from its end (TestLaneKeyWrites_BUG3057), so the two cannot drift into a
// state where this module is "correct" against a validator that has moved.
import { describe, it, expect } from 'vitest';
import { laneWriteValue, laneWriteRefusalMessage } from './laneWriteValue';
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

	it('gives a multi_select field a one-element ARRAY, and an empty one to clear', () => {
		// The picker OFFERS multi_select, so this is reachable with no retype.
		expect(laneWriteValue(f('multi_select'), 'c')).toEqual({ ok: true, value: ['c'] });
		expect(laneWriteValue(f('multi_select'), '')).toEqual({ ok: true, value: [] });
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
