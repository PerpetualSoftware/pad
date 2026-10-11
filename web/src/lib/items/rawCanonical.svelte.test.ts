import { describe, expect, it } from 'vitest';
import { openTabForms } from './rawCanonical';
import { toEditorMarkdown } from '$lib/utils/markdown';
import type { Item } from '$lib/types';

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

	// BUG-3582: the applier now converts a wiki-link to a link before
	// setContent, so an open tab stores the TITLE form `[[Fix it]]` as the
	// ref `[[TASK-5]]` (as the seed path always did). The prediction must
	// convert too, or a raw save through an open tab meets a false stale
	// dialog. Measured: without the conversion it predicts `[[Fix it]]`.
	it('predicts the ref form an open tab now stores for a title wiki-link', async () => {
		const index = [{ slug: 'fix-it', title: 'Fix it', item_number: 5, collection_prefix: 'TASK', collection_slug: 'tasks' }];
		const toEditor = (t: string) => toEditorMarkdown(t, index as unknown as Item[], 'docapp', 'dave');
		const [converted] = await openTabForms([toEditor('See [[Fix it]] now.')], index);
		const [unconverted] = await openTabForms(['See [[Fix it]] now.'], index);
		expect(converted).toBe('See [[TASK-5]] now.');
		expect(unconverted, 'control: the old prediction').toBe('See [[Fix it]] now.');
	});

	it('a ref wiki-link round-trips unchanged (the stored body survives)', async () => {
		const index = [{ slug: 'fix-it', title: 'Fix it', item_number: 5, collection_prefix: 'TASK', collection_slug: 'tasks' }];
		const toEditor = (t: string) => toEditorMarkdown(t, index as unknown as Item[], 'docapp', 'dave');
		expect(await openTabForms(['See [[TASK-5]] now.', 'See [[TASK-5|shown text]] now.'].map(toEditor), index)).toEqual([
			'See [[TASK-5]] now.',
			'See [[TASK-5|shown text]] now.',
		]);
	});
});
