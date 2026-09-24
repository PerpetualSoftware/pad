import { describe, expect, it } from 'vitest';
import type { Item } from '$lib/types';
import { countChildProgress, countedChildren } from './childProgress';

const child = (status: string) => ({ id: status, fields: JSON.stringify({ status }) }) as unknown as Item;
const terminal = ['done', 'cancelled'];
const abandoned = ['cancelled'];

describe('countChildProgress (BUG-3195)', () => {
	it('leaves an abandoned child out of both done and total', () => {
		expect(countChildProgress([child('done'), child('cancelled'), child('open')], terminal, abandoned)).toEqual({
			done: 1,
			total: 2
		});
	});

	it('gives 0/0 when every child is abandoned', () => {
		expect(countChildProgress([child('cancelled')], terminal, abandoned)).toEqual({ done: 0, total: 0 });
	});

	it('counts an abandoned value as done when nothing is declared abandoned', () => {
		// The control: the exclusion comes from the abandoned list, not from the
		// value being terminal.
		expect(countChildProgress([child('done'), child('cancelled')], terminal, [])).toEqual({ done: 2, total: 2 });
	});

	it('countedChildren keeps every non-abandoned child in order', () => {
		expect(countedChildren([child('open'), child('cancelled'), child('done')], abandoned).map((c) => c.id)).toEqual([
			'open',
			'done'
		]);
	});
});
