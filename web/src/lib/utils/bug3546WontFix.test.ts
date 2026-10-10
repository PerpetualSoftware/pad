import { describe, it, expect } from 'vitest';
import { statusColor } from '$lib/utils/fieldColors';
import { COLLECTION_TEMPLATES } from '$lib/components/collections/collection-templates';

// BUG-3546: the hyphenated `wont-fix` is the negative-terminal family on the
// board, and the web Bug Tracker template, which uses that spelling, declares
// what closes instead of relying on the fallback lists.
describe('wont-fix (BUG-3546)', () => {
	it('colours like wontfix', () => {
		expect(statusColor('wont-fix')).toBe(statusColor('wontfix'));
		expect(statusColor('wont-fix')).not.toBe(statusColor('open'));
	});

	it('the Bug Tracker template declares its terminal and abandoned options', () => {
		const status = COLLECTION_TEMPLATES.find((t) => t.id === 'bug-tracker')?.fields.find((f) => f.key === 'status');
		expect(status?.terminal_options).toEqual(['fixed', 'wont-fix']);
		expect(status?.abandoned_options).toEqual(['wont-fix']);
		for (const v of [...(status?.terminal_options ?? []), ...(status?.abandoned_options ?? [])]) {
			expect(status?.options).toContain(v);
		}
	});
});
