import { describe, expect, it } from 'vitest';
import {
	DROP_LINES_SHOWN,
	FIELD_CHARS,
	describeImportDrops,
	describeImportSummary,
	importDropLine
} from './importSummary';

const full = { collections: 9, items: 412, comments: 80, attachments: 37 };

describe('describeImportSummary', () => {
	it('reports a complete import with every count', () => {
		const r = describeImportSummary({ in_bundle: full, imported: full });
		expect(r.complete).toBe(true);
		expect(r.text).toBe(
			'9 collections, 412 items, 80 comments and 37 attachments, everything in the bundle.'
		);
	});

	it('names only the kinds that fell short', () => {
		const r = describeImportSummary({
			in_bundle: full,
			imported: { ...full, attachments: 36 }
		});
		expect(r.complete).toBe(false);
		expect(r.text).toContain("The counts don't match the bundle: 36 of 37 attachments arrived.");
		expect(r.text).not.toContain('items');
	});

	it('uses the singular for one', () => {
		const one = { collections: 1, items: 1, comments: 1, attachments: 1 };
		const r = describeImportSummary({ in_bundle: one, imported: { ...one, items: 0 } });
		expect(r.text).toContain('0 of 1 item arrived');
	});

	it('treats MORE arriving than the bundle carried as a mismatch too', () => {
		const r = describeImportSummary({
			in_bundle: full,
			imported: { ...full, collections: 10 }
		});
		expect(r.complete).toBe(false);
		expect(r.text).toContain('10 of 9 collections');
	});
});

describe('describeImportDrops (TASK-3842)', () => {
	const drop = (n: number) => ({ item: `TASK-${n}`, key: 'email', value: `a${n}@x.io`, holder: `TASK-${n + 100}` });

	it('says nothing when nothing was dropped', () => {
		expect(describeImportDrops(undefined)).toBeNull();
		expect(describeImportDrops([])).toBeNull();
	});

	it("matches the CLI's dropped line, one per drop", () => {
		expect(describeImportDrops([drop(1)])).toBe(
			'One unique value was dropped because another item already holds it:\n' +
				'dropped: TASK-1: email "a1@x.io" is taken by TASK-101'
		);
		expect(describeImportDrops([drop(1), drop(2)])?.split('\n')).toEqual([
			'2 unique values were dropped because other items already hold them:',
			'dropped: TASK-1: email "a1@x.io" is taken by TASK-101',
			'dropped: TASK-2: email "a2@x.io" is taken by TASK-102'
		]);
	});

	it('caps the lines and counts the rest', () => {
		const lines = describeImportDrops(Array.from({ length: DROP_LINES_SHOWN + 3 }, (_, i) => drop(i)))!.split('\n');
		expect(lines).toHaveLength(1 + DROP_LINES_SHOWN + 1);
		expect(lines[lines.length - 1]).toBe('and 3 more');
	});

	it('a line break in a server string cannot start a forged line', () => {
		const line = importDropLine({ item: 'TASK-1\ndropped: fake', key: 'k', value: 'v\nw', holder: 'TASK-2' });
		expect(line).not.toContain('\n');
		expect(line).toContain('"v\\nw"');
		expect(line).toContain('TASK-1\\u000adropped: fake');
	});
});

describe('importDropLine escapes and clips every field (TASK-3842 r2)', () => {
	const base = { item: 'TASK-1', key: 'email', value: 'a@x.io', holder: 'TASK-2' };
	const FIELDS = ['item', 'key', 'value', 'holder'] as const;
	// RLO (Cf), LINE SEPARATOR (Zl), NEXT LINE (Cc), ZERO WIDTH SPACE (Cf).
	const UNSAFE = [
		['‮', '\\u202e'],
		[' ', '\\u2028'],
		['\u0085', '\\u0085'],
		['​', '\\u200b']
	] as const;

	for (const f of FIELDS) {
		for (const [raw, escaped] of UNSAFE) {
			it(`escapes U+${raw.charCodeAt(0).toString(16).padStart(4, '0').toUpperCase()} in ${f}`, () => {
				const line = importDropLine({ ...base, [f]: `x${raw}y` });
				expect(line).not.toContain(raw);
				expect(line).toContain(`x${escaped}y`);
			});
		}
	}

	it('clips each raw field to FIELD_CHARS code points plus an ellipsis', () => {
		const long = 'a'.repeat(FIELD_CHARS + 10);
		const kept = 'a'.repeat(FIELD_CHARS);
		const line = importDropLine({ item: long, key: long, value: long, holder: long });
		expect(line).toBe(`dropped: ${kept}…: ${kept}… "${kept}…" is taken by ${kept}…`);
		// At the limit exactly, nothing is clipped.
		expect(importDropLine({ ...base, item: kept })).toContain(`dropped: ${kept}: `);
	});

	it('a cut beside an escapable character never splits its escape', () => {
		const head = 'a'.repeat(FIELD_CHARS - 1);
		// The last kept code point is U+202E: escaped whole, then the ellipsis.
		expect(importDropLine({ ...base, item: `${head}‮bbb` })).toContain(`${head}\\u202e…: `);
		// The first dropped code point is U+202E: it is gone, not half-escaped.
		const line = importDropLine({ ...base, item: `${head}b‮` });
		expect(line).toContain(`${head}b…: `);
		expect(line).not.toContain('\\u');
	});

	it('a cut never splits a surrogate pair', () => {
		const head = 'a'.repeat(FIELD_CHARS - 1);
		expect(importDropLine({ ...base, item: `${head}\u{1F600}x` })).toContain(`${head}\u{1F600}…: `);
	});
});
