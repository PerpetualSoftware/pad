import { describe, expect, it } from 'vitest';
import { describeImportSummary } from './importSummary';

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
