import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

/**
 * TASK-2921 — EVERY `syncService.onSync` subscriber compares `result.workspace`.
 *
 * The field's own doc comment says every subscriber must compare it, and when it
 * was introduced four of the five did not (codex round 9). So this is an
 * ENUMERATION test, not a list of per-site pins: it walks the source tree and
 * finds the subscribers itself, so a NEW one that skips the comparison fails
 * here without anyone remembering to add a case.
 *
 * The first draft of this file DID hardcode the list, and asserted its length —
 * which proves nothing, because a sixth subscriber in a file nobody listed is
 * exactly what the list cannot see. That is the same shape as the bug it guards.
 *
 * WHAT IT DOES NOT PROVE: that the comparison is REACHED, or that `wsSlug` is
 * the right thing to compare against in each component. It is a source scan and
 * inherits every limit the layout wiring pin records.
 */
const SRC = fileURLToPath(new URL('../..', import.meta.url));

function walk(dir: string, out: string[] = []): string[] {
	for (const entry of readdirSync(dir)) {
		const full = join(dir, entry);
		if (statSync(full).isDirectory()) walk(full, out);
		else if (/\.(svelte|ts)$/.test(entry) && !/\.test\.ts$/.test(entry)) out.push(full);
	}
	return out;
}

const subscribers = walk(SRC).filter((f) =>
	readFileSync(f, 'utf8').includes('syncService.onSync'),
);

describe('every onSync subscriber filters by the result’s workspace', () => {
	it('finds the subscribers by scanning, not from a list', () => {
		// If this ever reads zero, the scan broke and every assertion below is
		// passing vacuously — the failure mode a per-file loop hides.
		expect(subscribers.length).toBeGreaterThan(0);
	});

	for (const file of subscribers) {
		const rel = file.slice(SRC.length);
		it(`${rel} compares result.workspace`, () => {
			expect(readFileSync(file, 'utf8')).toContain('result.workspace');
		});
	}
});
