import { describe, expect, it } from 'vitest';
import { DROP_LINES_SHOWN, describeImportDrops, describeImportSummary, importDropLine } from './importSummary';

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
