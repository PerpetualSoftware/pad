import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * BUG-3230 U0 — the pane's own content writes ask to be REFUSED rather than
 * replace another tab's unstored edits. A population pin over the source: every
 * `api.items.update` call that sends `content:` must also send
 * `refuse_pending_edits: true`. The collab-snapshot flush goes through
 * `flushCollabContent`, not `update`, and is exempt server-side (it is the tab
 * writing those edits). Behaviour is pinned by
 * e2e/bug-3230-raw-save-pending-edits.spec.ts; this catches a new content door
 * added without the flag, which the e2e would not exercise.
 */
const SOURCE = readFileSync(fileURLToPath(new URL('./ItemDetail.svelte', import.meta.url)), 'utf-8');

/** The argument text of each `.update(` call on api.items, by paren matching. */
function updateCalls(src: string): string[] {
	const out: string[] = [];
	for (const m of src.matchAll(/api\.items\s*\.update\(/g)) {
		const open = m.index! + m[0].length - 1;
		let depth = 0;
		for (let i = open; i < src.length; i++) {
			if (src[i] === '(') depth++;
			else if (src[i] === ')' && --depth === 0) {
				out.push(src.slice(open, i + 1));
				break;
			}
		}
	}
	return out;
}

describe('ItemDetail content writes refuse over unstored edits (BUG-3230 U0)', () => {
	const calls = updateCalls(SOURCE);
	const content = calls.filter((c) => /\bcontent:/.test(c));

	it('the population is the five content doors', () => {
		// The raw debounce, the raw unload flush, the raw drain, the legacy rich
		// save, and restoring a kept draft.
		expect(content.length).toBe(5);
	});

	it.each(content.map((c, i) => [i, c.slice(0, 120).replace(/\s+/g, ' ')] as const))(
		'content door %i sends refuse_pending_edits: %s',
		(i) => {
			expect(content[i]).toMatch(/refuse_pending_edits: true/);
		}
	);
});
