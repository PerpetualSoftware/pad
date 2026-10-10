import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join, relative } from 'node:path';
import { parse } from 'svelte/compiler';
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

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type Node = any;

function walk(node: Node, visit: (n: Node) => void): void {
	if (!node || typeof node !== 'object') return;
	if (Array.isArray(node)) {
		for (const child of node) walk(child, visit);
		return;
	}
	if (typeof node.type === 'string') visit(node);
	for (const [k, v] of Object.entries(node)) if (k !== 'parent') walk(v, visit);
}

/**
 * The keyed each blocks of one file, read by Svelte's own parser (codex
 * rounds 1-2: a hand scanner misread braces in strings, comments and regex
 * literals, and comment or script text). The id keeps the census format:
 * `file :: each <expr> as <item>[, <index>] (<key>)`, whitespace collapsed,
 * with `#n` on the nth repeat of an identical head in one file.
 */
export function keyedEaches(file: string, src: string): (Site & { keyNode: Node; index?: string })[] {
	const out: (Site & { keyNode: Node; index?: string })[] = [];
	const seen = new Map<string, number>();
	walk(parse(src, { modern: true }).fragment, (n) => {
		if (n.type !== 'EachBlock' || !n.key) return;
		const text = (x: Node) => collapse(src.slice(x.start, x.end));
		const item = n.context ? text(n.context) + (n.index ? `, ${n.index}` : '') : n.index ?? '';
		const head = `${file} :: each ${text(n.expression)} as ${item} (${text(n.key)})`;
		const count = (seen.get(head) ?? 0) + 1;
		seen.set(head, count);
		out.push({
			file,
			line: src.slice(0, n.start).split('\n').length,
			id: count === 1 ? head : `${head} #${count}`,
			item,
			key: text(n.key),
			keyNode: n.key,
			index: n.index ?? undefined,
		});
	});
	return out.sort((x, y) => x.line - y.line);
}

const endsWithNonDigit = (s: unknown) => typeof s === 'string' && s.length > 0 && !/[0-9]$/.test(s);

/**
 * The key is position-safe, read from its AST: exactly the index; a template
 * literal ending in `<non-digit>${index}`; `… + '<non-digit>' + index`; or
 * JSON.stringify([…, index]). With the index LAST and after a non-digit
 * separator, the trailing digits recover it, so two positions cannot share a
 * key. `${i}${x}`, `x.id + i`, `i % 2`, `row.i`, a conditional or a function
 * of the index are not, and need an entry (codex round 2).
 */
export function keysOnPosition(keyNode: Node, index: string | undefined): boolean {
	if (!index) return false;
	const isIndex = (x: Node) => x?.type === 'Identifier' && x.name === index;
	if (isIndex(keyNode)) return true;
	if (keyNode.type === 'TemplateLiteral') {
		const exprs = keyNode.expressions;
		const quasis = keyNode.quasis;
		return (
			exprs.length > 0 &&
			isIndex(exprs[exprs.length - 1]) &&
			quasis[quasis.length - 1].value.cooked === '' &&
			endsWithNonDigit(quasis[quasis.length - 2].value.cooked)
		);
	}
	if (keyNode.type === 'BinaryExpression' && keyNode.operator === '+' && isIndex(keyNode.right)) {
		const left = keyNode.left;
		const sep = left.type === 'BinaryExpression' && left.operator === '+' ? left.right : left;
		return sep?.type === 'Literal' && endsWithNonDigit(sep.value);
	}
	if (
		keyNode.type === 'CallExpression' &&
		keyNode.callee?.type === 'MemberExpression' &&
		keyNode.callee.object?.name === 'JSON' &&
		keyNode.callee.property?.name === 'stringify' &&
		keyNode.arguments[0]?.type === 'ArrayExpression'
	) {
		const els = keyNode.arguments[0].elements;
		return isIndex(els[els.length - 1]);
	}
	return false;
}

const sites = svelteFiles(SRC).flatMap((p) => keyedEaches(relative(SRC, p), readFileSync(p, 'utf8')));
const reasons = Object.keys(EACH_KEY_REASONS).join(' | ');

describe('keyed {#each} census (TASK-3539)', () => {
	it('finds the keyed eaches it is meant to (premise)', () => {
		expect(sites.length).toBeGreaterThan(250);
	});

	it('every keyed each that does not key on its position names why its keys cannot repeat', () => {
		const missing = sites
			.filter((s) => !keysOnPosition(s.keyNode, s.index) && !(s.id in EACH_KEY_CENSUS))
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

	it('the scan reads multi-line heads, strings, comments and repeats right (control)', () => {
		const src = [
			'<script>const s = "{#each fake as f (f)}";</script>',
			'<!-- {#each hidden as h (h)} -->',
			'{#each rows.filter((r) =>\n  r.ok) as { a, b }, i (a.id)}x{/each}',
			'{#each xs.filter((x) => x.name !== "}") as x (x.id /* } */)}a{/each}',
			'{#each ys.filter((y) => /}/.test(y)) as y (y)}b{/each}',
			'{#each zs as z (z.id)}c{/each}',
			'{#each zs as z (z.id)}d{/each}',
			'{#each ns as n}e{/each}',
		].join('\n');
		expect(keyedEaches('f.svelte', src).map((s) => s.id)).toEqual([
			'f.svelte :: each rows.filter((r) => r.ok) as { a, b }, i (a.id)',
			'f.svelte :: each xs.filter((x) => x.name !== "}") as x (x.id)',
			'f.svelte :: each ys.filter((y) => /}/.test(y)) as y (y)',
			'f.svelte :: each zs as z (z.id)',
			'f.svelte :: each zs as z (z.id) #2',
		]);
	});

	it('only a position-safe key is exempt (codex rounds 1-2)', () => {
		const exempt = (key: string) => {
			const [site] = keyedEaches('f.svelte', `{#each xs as x, i (${key})}a{/each}`);
			return keysOnPosition(site.keyNode, site.index);
		};
		expect(exempt('i')).toBe(true);
		expect(exempt('`${x.id}:${i}`')).toBe(true);
		expect(exempt("x.id + ':' + i")).toBe(true);
		expect(exempt('JSON.stringify([x.kind, i])')).toBe(true);
		expect(exempt('`${i}${x.suffix}`')).toBe(false);
		expect(exempt('`${x.id}${i}`')).toBe(false);
		expect(exempt('`v1${i}`')).toBe(false);
		expect(exempt('x.id + i')).toBe(false);
		expect(exempt('i % 2')).toBe(false);
		expect(exempt('x.i')).toBe(false);
		expect(exempt("i > 0 ? 'rest' : 'first'")).toBe(false);
		expect(exempt('rowKey(x, i)')).toBe(false);
	});
});
