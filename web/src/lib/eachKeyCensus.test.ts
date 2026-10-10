import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join, relative } from 'node:path';
import { EACH_KEY_CENSUS, EACH_KEY_REASONS } from './eachKeyCensus';

/**
 * TASK-3539: every keyed {#each} in web/src either keys on its own position
 * or names, in eachKeyCensus.ts, why its keys cannot repeat. A repeated key
 * throws each_key_duplicate during render in Svelte 5 production builds and
 * blanks the whole view (BUG-3538: the dashboard never left its skeleton).
 *
 * The scan reads each {#each …} head whole (it may span lines), takes the
 * trailing parenthesised key, and normalises whitespace. Scope, stated: an
 * each built by a snippet or a component that renders its own keyed list is
 * scanned in THAT file, not where it is called.
 */

const SRC = fileURLToPath(new URL('..', import.meta.url));

interface Site {
	file: string;
	line: number;
	id: string;
	item: string;
	key: string;
}

function svelteFiles(dir: string): string[] {
	const out: string[] = [];
	for (const e of readdirSync(dir, { withFileTypes: true })) {
		const p = join(dir, e.name);
		if (e.isDirectory()) out.push(...svelteFiles(p));
		else if (e.name.endsWith('.svelte')) out.push(p);
	}
	return out;
}

const collapse = (s: string) => s.split(/\s+/).filter(Boolean).join(' ');

/** The keyed each blocks of one file's source. */
export function keyedEaches(file: string, src: string): Site[] {
	const out: Site[] = [];
	let from = 0;
	for (;;) {
		const start = src.indexOf('{#each', from);
		if (start < 0) return out;
		let depth = 0;
		let end = start;
		for (; end < src.length; end++) {
			if (src[end] === '{') depth++;
			else if (src[end] === '}' && --depth === 0) break;
		}
		from = end;
		const head = src.slice(start + 6, end).trim();
		if (!head.endsWith(')')) continue;
		let d = 0;
		let open = head.length - 1;
		for (; open >= 0; open--) {
			if (head[open] === ')') d++;
			else if (head[open] === '(' && --d === 0) break;
		}
		const before = head.slice(0, open).trimEnd();
		const as = before.match(/^([\s\S]*)\s+as\s+([\s\S]*)$/);
		if (!as) continue;
		const expr = collapse(as[1]);
		const item = collapse(as[2]);
		const key = collapse(head.slice(open + 1, -1));
		out.push({
			file,
			line: src.slice(0, start).split('\n').length,
			id: `${file} :: each ${expr} as ${item} (${key})`,
			item,
			key,
		});
	}
}

/** The key is, or contains, the each's index variable: a position cannot repeat. */
export function keysOnPosition(site: Pick<Site, 'item' | 'key'>): boolean {
	const parts = site.item.split(/,(?![^[{]*[\]}])/).map((s) => s.trim());
	const index = parts[1];
	return !!index && new RegExp(`\\b${index.replace(/[$]/g, '\\$')}\\b`).test(site.key);
}

const sites = svelteFiles(SRC).flatMap((p) => keyedEaches(relative(SRC, p), readFileSync(p, 'utf8')));
const reasons = Object.keys(EACH_KEY_REASONS).join(' | ');

describe('keyed {#each} census (TASK-3539)', () => {
	it('finds the keyed eaches it is meant to (premise)', () => {
		expect(sites.length).toBeGreaterThan(250);
	});

	it('every keyed each that does not key on its position names why its keys cannot repeat', () => {
		const missing = sites
			.filter((s) => !keysOnPosition(s) && !(s.id in EACH_KEY_CENSUS))
			.map(
				(s) =>
					`${s.file}:${s.line} has a keyed each with no census entry. Add to web/src/lib/eachKeyCensus.ts:\n` +
					`\t${JSON.stringify(s.id)}: { reason: '<${reasons}>', note: '<where the list comes from, and why its keys cannot repeat>' },`,
			);
		expect(missing, missing.join('\n\n')).toEqual([]);
	});

	it('every census entry still describes a keyed each in the source (no stale entries)', () => {
		const live = new Set(sites.map((s) => s.id));
		const stale = Object.keys(EACH_KEY_CENSUS).filter((id) => !live.has(id));
		expect(stale, `stale census entries (the each changed or is gone; update or remove them):\n${stale.join('\n')}`).toEqual([]);
	});

	it('every entry names a reason from the closed set, with a note', () => {
		const bad = Object.entries(EACH_KEY_CENSUS)
			.filter(([, e]) => !(e.reason in EACH_KEY_REASONS) || e.note.trim() === '')
			.map(([id]) => id);
		expect(bad).toEqual([]);
	});

	it('the scan reads a multi-line head, a destructured item and a position key (control)', () => {
		const src = `{#each rows.filter((r) =>\n  r.ok) as { a, b }, i (a.id)}x{/each}\n{#each xs as x, n (\`\${x}:\${n}\`)}y{/each}\n{#each ys as y}z{/each}`;
		const got = keyedEaches('f.svelte', src);
		expect(got.map((s) => s.id)).toEqual([
			'f.svelte :: each rows.filter((r) => r.ok) as { a, b }, i (a.id)',
			'f.svelte :: each xs as x, n (`${x}:${n}`)',
		]);
		expect(got.map(keysOnPosition)).toEqual([false, true]);
	});
});
