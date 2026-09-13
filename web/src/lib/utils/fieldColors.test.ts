// The canonical-palette helpers are TOTAL over what a fields blob can hold
// (BUG-3041).
//
// These five functions are handed values straight out of an item's `fields`
// JSON. Every one of them was typed `string` and none of them checked, so an
// array reached `.toLowerCase()` / `.replace()` and RENDERING THREW —
// `value?.toLowerCase is not a function` — taking down the whole board or table
// rather than one chip. A declared field type is not a promise about the stored
// value: nothing rewrites existing data when a field is retyped in the schema
// editor, so a `status` that was a `multi_select` can hold an array while its
// schema says `multi_relation`.
//
// THE INPUT DOMAIN IS JSON, not "string or nothing", so the legs below are
// drawn from what JSON can actually put in that blob — array, number, boolean,
// object, null, undefined — rather than from the one non-string value the
// original report happened to name.
import { describe, it, expect } from 'vitest';
import {
	statusColor,
	priorityColor,
	hasCanonicalStatus,
	formatFieldLabel,
	columnAccentClassFor,
	canonicalValueColor,
} from './fieldColors';

/** Everything a `fields` blob can hold that is not a string. */
const NON_STRINGS: [string, unknown][] = [
	['an array of references', ['uuid-a', 'uuid-b']],
	['an empty array', []],
	['a number', 42],
	['a boolean', true],
	['an object', { a: 1 }],
	['null', null],
	['undefined', undefined],
];

describe('the palette helpers do not throw on a non-string value', () => {
	for (const [label, value] of NON_STRINGS) {
		it(`answers a USABLE colour for ${label}`, () => {
			// Not-throwing was the whole assertion here at first, and the
			// enumeration round pointed out that returning `undefined`, an object,
			// or a wrong colour would all have passed. The colour helpers always
			// return a colour, so the honest one for an unreadable value is muted.
			expect(statusColor(value)).toBe(MUTED);
			expect(priorityColor(value)).toBe(MUTED);
		});

		it(`survives ${label}`, () => {
			// Each assertion is the CALL not throwing plus a usable answer: a
			// helper that threw would fail before any expectation ran, which is
			// the failure mode this file exists for.
			expect(() => statusColor(value)).not.toThrow();
			expect(() => priorityColor(value)).not.toThrow();
			expect(() => hasCanonicalStatus(value)).not.toThrow();
			expect(() => formatFieldLabel(value)).not.toThrow();
			expect(() => columnAccentClassFor(undefined, value)).not.toThrow();
			expect(() => canonicalValueColor(value)).not.toThrow();
		});

		it(`reads ${label} as "no canonical value", not as some value`, () => {
			// Not-throwing is not enough: answering as though the value WERE a
			// known status would colour a chip by accident. The honest answer for
			// something this palette cannot read is the unknown one.
			expect(hasCanonicalStatus(value)).toBe(false);
			expect(canonicalValueColor(value)).toBeNull();
			expect(formatFieldLabel(value)).toBe('');
			expect(columnAccentClassFor({ terminal_options: ['done'] }, value)).toBe('');
		});
	}
});

/**
 * The palette's actual values, written out.
 *
 * The first version of this file asserted RELATIONSHIPS — that two canonical
 * statuses agree, that a priority equals what the shared resolver returns —
 * and the enumeration round showed why that is not enough: forcing every
 * priority colour to muted left every leg green, because the assertions
 * compared the implementation against itself. A palette test has to name the
 * colours (harness note `end-state-assertions-need-a-counterfactual`).
 */
const GREEN = 'var(--accent-green)';
const AMBER = 'var(--accent-amber)';
const BLUE = 'var(--status-blue)';
const ORANGE = 'var(--accent-orange)';
const GRAY = 'var(--accent-gray)';
const MUTED = 'var(--text-muted)';
const SECONDARY = 'var(--text-secondary)';

describe('CONTROL: strings still resolve exactly as before', () => {
	// Without these, making every helper answer '' for everything would satisfy
	// the file above completely.
	it('canonical statuses keep their exact colours', () => {
		expect(hasCanonicalStatus('in_progress')).toBe(true);
		expect(hasCanonicalStatus('in-progress')).toBe(true);
		expect(statusColor('done')).toBe(GREEN);
		expect(statusColor('completed')).toBe(GREEN);
		expect(statusColor('in_progress')).toBe(AMBER);
		expect(statusColor('open')).toBe(BLUE);
		expect(statusColor('blocked')).toBe(ORANGE);
		expect(statusColor('cancelled')).toBe(GRAY);
	});

	it('an UNKNOWN string reads muted rather than borrowing a palette entry', () => {
		expect(statusColor('shipped')).toBe(MUTED);
	});

	it('an unknown string is unknown, which is not the same as unreadable', () => {
		// The distinction the non-string legs turn on: 'shipped' is a value the
		// palette read and did not recognise; an array is one it cannot read.
		// Both end at "no canonical colour", and only one of them used to crash.
		expect(hasCanonicalStatus('shipped')).toBe(false);
		expect(canonicalValueColor('shipped')).toBeNull();
		expect(columnAccentClassFor({ terminal_options: ['shipped'] }, 'shipped')).toBe('col-done');
	});

	it('priorities keep their exact colours, not merely the helper\'s own answer', () => {
		// Named values, for the reason above: comparing `canonicalValueColor` to
		// `priorityColor` passes however wrong both of them are together.
		expect(priorityColor('critical')).toBe(ORANGE);
		expect(priorityColor('high')).toBe(AMBER);
		expect(priorityColor('medium')).toBe(SECONDARY);
		expect(priorityColor('low')).toBe(MUTED);
		expect(canonicalValueColor('Medium')).toBe(SECONDARY);
		expect(canonicalValueColor('critical')).toBe(ORANGE);
	});

	it('critical is ORANGE and not red — red is reserved for destructive actions', () => {
		// A long-standing app convention that a "make it louder" edit would break
		// without any other leg noticing.
		expect(priorityColor('critical')).not.toContain('danger');
		expect(priorityColor('critical')).not.toContain('error');
	});

	it('formats underscores and casing the way the chips expect', () => {
		expect(formatFieldLabel('in_progress')).toBe('In Progress');
		expect(formatFieldLabel('done')).toBe('Done');
	});

	it('an EMPTY string stays empty rather than becoming a label', () => {
		expect(formatFieldLabel('')).toBe('');
		expect(canonicalValueColor('')).toBeNull();
	});
});

describe('canonicalValueColor is the one copy of a question that had two', () => {
	it('answers the status palette for a word in BOTH vocabularies', () => {
		// The precedence claim, stated honestly. The two vocabularies are
		// currently DISJOINT — no canonical status is also a priority word — so
		// no fixture built from them can distinguish "statuses first" from
		// "priorities first", and the leg that used 'blocked' to prove ordering
		// proved nothing (enumeration round, d6).
		//
		// What CAN be pinned is the observable consequence: 'blocked' is a status
		// and takes the status colour, 'high' is a priority and takes the
		// priority one. If the vocabularies ever overlap, whoever adds the
		// overlapping word owns the ordering question, and this comment is where
		// they will find it.
		expect(canonicalValueColor('blocked')).toBe(ORANGE);
		expect(canonicalValueColor('high')).toBe(AMBER);
		expect(statusColor('high')).toBe(MUTED);
	});

	it('leaves a value in neither vocabulary to plain text', () => {
		expect(canonicalValueColor('needs-triage')).toBeNull();
	});
});

describe('a value that happens to name an OBJECT PROPERTY is not a status', () => {
	// `STATUS_COLORS` is an object literal, so `'__proto__' in STATUS_COLORS` is
	// true and `STATUS_COLORS['constructor']` is a FUNCTION. Both are reachable
	// from a plain string a user can type into a text field, and neither is a
	// CSS value — so the helpers answered yes to a status they have never heard
	// of and handed their callers something unusable (enumeration round, a16).
	for (const key of ['__proto__', 'constructor', 'toString', 'hasOwnProperty', 'valueOf']) {
		it(`treats ${key} as an ordinary unknown value`, () => {
			expect(hasCanonicalStatus(key)).toBe(false);
			expect(statusColor(key)).toBe(MUTED);
			expect(typeof statusColor(key)).toBe('string');
			expect(canonicalValueColor(key)).toBeNull();
			expect(columnAccentClassFor({ terminal_options: [] }, key)).toBe('');
		});
	}
});
