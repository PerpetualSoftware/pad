import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import type { Collection, Item } from '$lib/types';
import { childState, countChildProgress } from './childProgress';

// The web half of the progress parity vector (BUG-3195). KEEP-IN-SYNC: the Go
// half is internal/models/progress_abandoned_parity_test.go, and both assert
// testdata/progress_abandoned.json rather than each other.

const VECTOR_PATH = fileURLToPath(new URL('../../../../testdata/progress_abandoned.json', import.meta.url));

type Expect = { states: string[]; done: number; total: number };
type Case = { name: string; schema: string; settings: string; children: Record<string, unknown>[]; web: Expect };

const cases = (JSON.parse(readFileSync(VECTOR_PATH, 'utf8')) as { cases: Case[] }).cases;

describe('progress parity vector (BUG-3195)', () => {
	it('reads the whole vector', () => {
		expect(cases.length).toBeGreaterThanOrEqual(7);
	});

	for (const c of cases) {
		it(c.name, () => {
			const collection = { slug: 'v', schema: c.schema, settings: c.settings } as unknown as Collection;
			const children = c.children.map(
				(f, i) => ({ id: `c${i}`, collection_slug: 'v', fields: JSON.stringify(f) }) as unknown as Item
			);
			expect(children.map((ch) => childState(ch, collection))).toEqual(c.web.states);
			expect(countChildProgress(children, [collection])).toEqual({ done: c.web.done, total: c.web.total });
		});
	}
});
