import { describe, expect, it } from 'vitest';
import { openTabForms } from './rawCanonical';

// BUG-3540: the stored form an open rich tab writes for a raw save's text.
describe('openTabForms', () => {
	it("rewrites what the editor rewrites ('* ' bullets become '- ')", async () => {
		const [form] = await openTabForms(['Original body.\n\n* one\n* two'], []);
		expect(form).toBe('Original body.\n\n- one\n- two');
	});

	it('leaves canonical text as it is', async () => {
		expect(await openTabForms(['Original body. one two'], [])).toEqual(['Original body. one two']);
	});

	it('one form per text, in order', async () => {
		const forms = await openTabForms(['a', '* b'], []);
		expect(forms).toEqual(['a', '- b']);
	});
});
