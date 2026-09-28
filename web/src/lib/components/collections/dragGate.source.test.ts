/**
 * BUG-3158 — every card/row/group drag zone in the collection views asks ONE
 * question, `viewport.dragDisabled` (narrow viewport OR coarse primary pointer).
 * The board was gated on width alone and the list not at all, and both let a
 * held finger drag an item into another lane or group.
 *
 * A SOURCE guard, because the dnd config is not observable in jsdom (the zones'
 * behaviour is pinned end to end by web/e2e/bug-3158-*.spec.ts, which covers the
 * board and the list's item zone but not the group-reorder zone). It enumerates
 * the zones from the source itself, so a NEW zone that forgets the gate fails
 * here, and the count precondition stops an empty match from passing.
 */
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// Paths relative to this directory, with the number of zones each must hold.
// BUG-3159 added the roles board and the child list.
const VIEWS = {
	'BoardView.svelte': 1,
	'ListView.svelte': 2,
	'../ChildItems.svelte': 1,
	'../../../routes/[username]/[workspace]/roles/+page.svelte': 1,
} as const;

/**
 * The text of each `use:dndzone={{ … }}` or `use:lockableDndzone={{ … }}`
 * config, brace-matched. `lockableDndzone` is `dndzone` with per-card locking
 * (BUG-3259) and takes the same options, so it is the same zone for this guard.
 */
function dndzoneConfigs(src: string): string[] {
	const out: string[] = [];
	const opener = /use:(?:lockableDndzone|dndzone)=\{\{/g;
	for (let m = opener.exec(src); m; m = opener.exec(src)) {
		let depth = 0;
		let i = m.index + m[0].length - 2;
		const start = i;
		for (; i < src.length; i++) {
			if (src[i] === '{') depth++;
			else if (src[i] === '}' && --depth === 0) break;
		}
		out.push(src.slice(start, i + 1));
		opener.lastIndex = i;
	}
	return out;
}

describe('every item drag zone asks viewport.dragDisabled', () => {
	for (const [file, expected] of Object.entries(VIEWS)) {
		it(file, () => {
			const src = readFileSync(resolve(__dirname, file), 'utf8');
			const configs = dndzoneConfigs(src);
			expect(configs.length, `expected ${expected} dnd zone(s) in ${file}`).toBe(expected);
			for (const cfg of configs) {
				const line = cfg.split('\n').find((l) => /^\s*dragDisabled:/.test(l)) ?? '';
				expect(line, `a zone in ${file} has no dragDisabled`).not.toBe('');
				// BoardView reads it through its `noTouchDrag` derived.
				expect(line, `a zone in ${file} is not gated on viewport.dragDisabled`).toMatch(
					/viewport\.dragDisabled|noTouchDrag/,
				);
			}
		});
	}

	it('BoardView’s noTouchDrag is exactly viewport.dragDisabled', () => {
		const src = readFileSync(resolve(__dirname, 'BoardView.svelte'), 'utf8');
		expect(src).toMatch(/const noTouchDrag = \$derived\(viewport\.dragDisabled\);/);
	});
});
