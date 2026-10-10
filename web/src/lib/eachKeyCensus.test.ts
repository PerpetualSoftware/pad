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

/**
 * Blanks HTML comments and <script> / <style> blocks (keeping newlines, so line
 * numbers hold): an {#each} written there is not markup.
 */
export function markupOnly(src: string): string {
	return src.replace(/<!--[\s\S]*?-->|<script\b[\s\S]*?<\/script>|<style\b[\s\S]*?<\/style>/g, (m) =>
		m.replace(/[^\n]/g, ' '),
	);
}

/**
 * The index of the `}` closing the tag that opens at `start`, skipping braces
 * inside '…', "…" and `…` strings (a template literal's ${…} counts as code).
 */
function closingBrace(src: string, start: number): number {
	let depth = 0;
	const templates: number[] = []; // brace depth at each open ${ inside a template
	let quote = '';
	for (let i = start; i < src.length; i++) {
		const c = src[i];
		if (quote) {
			if (c === '\\') i++;
			else if (quote === '`' && c === '$' && src[i + 1] === '{') {
				templates.push(depth);
				depth++;
				i++;
				quote = '';
			} else if (c === quote) quote = '';
			continue;
		}
		if (c === "'" || c === '"' || c === '`') quote = c;
		else if (c === '{') depth++;
		else if (c === '}') {
			depth--;
			if (templates.length && templates[templates.length - 1] === depth) {
				templates.pop();
				quote = '`';
				continue;
			}
			if (depth === 0) return i;
		}
	}
	return src.length;
}

/** The keyed each blocks of one file's source. */
export function keyedEaches(file: string, raw: string): Site[] {
	const src = markupOnly(raw);
	const out: Site[] = [];
	const seen = new Map<string, number>();
	let from = 0;
	for (;;) {
		const start = src.indexOf('{#each', from);
		if (start < 0) return out;
		const end = closingBrace(src, start);
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
		const head1 = `${file} :: each ${expr} as ${item} (${key})`;
		// A repeated identical head in one file gets an ordinal, so a new copy
		// needs its own entry rather than riding on the first one's.
		const n = (seen.get(head1) ?? 0) + 1;
		seen.set(head1, n);
		out.push({ file, line: raw.slice(0, start).split('\n').length, id: n === 1 ? head1 : `${head1} #${n}`, item, key });
	}
}

/**
 * The key is position-safe: exactly the index, the index interpolated into a
 * template literal (`${x}:${i}`), a trailing `+ i`, or the index as an array
 * element (JSON.stringify([..., i])). Arithmetic on it (`i % 2`), a property
 * named like it (`row.i`) or a conditional is NOT, and needs an entry.
 */
export function keysOnPosition(site: Pick<Site, 'item' | 'key'>): boolean {
	const parts = site.item.split(/,(?![^[{]*[\]}])/).map((s) => s.trim());
	const index = parts[1];
	if (!index || !/^[A-Za-z_$][\w$]*$/.test(index)) return false;
	const k = site.key;
	const i = index.replace(/\$/g, '\\$');
	return (
		k === index ||
		(k.startsWith('`') && new RegExp(`\\$\\{${i}\\}`).test(k)) ||
		new RegExp(`[^.\\w$]\\+\\s*${i}$`).test(k) ||
		new RegExp(`JSON\\.stringify\\(\\[[\\s\\S]*,\\s*${i}\\s*\\]\\)$`).test(k)
	);
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
					`\t${JSON.stringify(s.id)}: { reason: '???', note: '???' },\n` +
					`(replace the first ??? with one of: ${reasons}; the second with where the list comes from and why its keys cannot repeat)`,
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

	it('braces in strings, comments, scripts and repeated heads do not fool the scan (codex round 1)', () => {
		const src = [
			'{#each xs.filter((x) => x.name !== "}") as x (x.id)}a{/each}',
			'{#each ys.map((y) => `${y.a}{`) as y (y)}b{/each}',
			'<!-- {#each hidden as h (h)} -->',
			'<script>const s = "{#each fake as f (f)}";</script>',
			'{#each zs as z (z.id)}c{/each}',
			'{#each zs as z (z.id)}d{/each}',
		].join('\n');
		expect(keyedEaches('f.svelte', src).map((s) => s.id)).toEqual([
			'f.svelte :: each xs.filter((x) => x.name !== "}") as x (x.id)',
			'f.svelte :: each ys.map((y) => `${y.a}{`) as y (y)',
			'f.svelte :: each zs as z (z.id)',
			'f.svelte :: each zs as z (z.id) #2',
		]);
	});

	it('only a position-safe key is exempt (codex round 1)', () => {
		const exempt = (item: string, key: string) => keysOnPosition({ item, key });
		expect(exempt('x, i', 'i')).toBe(true);
		expect(exempt('x, i', '`${x.id}:${i}`')).toBe(true);
		expect(exempt('x, i', "x.id + ':' + i")).toBe(true);
		expect(exempt('x, i', 'JSON.stringify([x.kind, i])')).toBe(true);
		expect(exempt('x, i', 'i % 2')).toBe(false);
		expect(exempt('x, i', 'x.i')).toBe(false);
		expect(exempt('x, i', "i > 0 ? 'rest' : 'first'")).toBe(false);
		expect(exempt('x, i', 'rowKey(x, i)')).toBe(false);
		expect(exempt('x', 'x.id')).toBe(false);
	});
});
