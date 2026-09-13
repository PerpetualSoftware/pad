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

describe('CONTROL: strings still resolve exactly as before', () => {
	// Without these, making every helper answer '' for everything would satisfy
	// the file above completely.
	it('canonical statuses keep their colours', () => {
		expect(hasCanonicalStatus('in_progress')).toBe(true);
		expect(hasCanonicalStatus('in-progress')).toBe(true);
		expect(statusColor('done')).toBe(statusColor('completed'));
		expect(statusColor('open')).not.toBe(statusColor('done'));
	});

	it('an unknown string is unknown, which is not the same as unreadable', () => {
		// The distinction the non-string legs turn on: 'shipped' is a value the
		// palette read and did not recognise; an array is one it cannot read.
		// Both end at "no canonical colour", and only one of them used to crash.
		expect(hasCanonicalStatus('shipped')).toBe(false);
		expect(canonicalValueColor('shipped')).toBeNull();
		expect(columnAccentClassFor({ terminal_options: ['shipped'] }, 'shipped')).toBe('col-done');
	});

	it('priorities keep their colours through the shared resolver', () => {
		expect(canonicalValueColor('critical')).toBe(priorityColor('critical'));
		expect(canonicalValueColor('high')).toBe(priorityColor('high'));
		expect(canonicalValueColor('Medium')).toBe(priorityColor('medium'));
		expect(canonicalValueColor('low')).toBe(priorityColor('low'));
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

describe('canonicalValueColor is the one copy of a question that had three', () => {
	it('prefers the status palette over the priority words', () => {
		// 'blocked' is a canonical STATUS and not a priority; if the priority arm
		// ran first, a blocked lane would take a priority colour. The order is
		// load-bearing and was duplicated into TableView and FieldEditor, where a
		// future edit to one copy would have moved only that surface.
		expect(canonicalValueColor('blocked')).toBe(statusColor('blocked'));
	});

	it('leaves a value in neither vocabulary to plain text', () => {
		expect(canonicalValueColor('needs-triage')).toBeNull();
	});
});
