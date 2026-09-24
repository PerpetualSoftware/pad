import { describe, expect, it } from 'vitest';
import { getAbandonedOptions, DEFAULT_ABANDONED_STATUSES, type Collection, type Item } from '$lib/types';
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

describe('getAbandonedOptions (BUG-3195)', () => {
	const coll = (field: Record<string, unknown>) =>
		({ schema: JSON.stringify({ fields: [{ key: 'status', type: 'select', ...field }] }) }) as unknown as Collection;

	it('uses the status field’s own abandoned_options when declared', () => {
		expect(getAbandonedOptions(coll({ terminal_options: ['done', 'overturned'], abandoned_options: ['overturned'] }))).toEqual([
			'overturned'
		]);
	});

	it('falls back to the terminal options that are negative outcomes', () => {
		expect(getAbandonedOptions(coll({ terminal_options: ['fixed', 'wontfix', 'duplicate'] }))).toEqual(['wontfix', 'duplicate']);
	});

	it('with no terminal options, applies the fallback to the default terminal list', () => {
		expect(getAbandonedOptions(coll({}))).toEqual(DEFAULT_ABANDONED_STATUSES);
		expect(DEFAULT_ABANDONED_STATUSES).toEqual(['cancelled', 'rejected', 'wontfix', 'disabled']);
	});
});
