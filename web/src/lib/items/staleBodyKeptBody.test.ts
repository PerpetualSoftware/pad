import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// BUG-3050 U3 (codex round 1): a site that builds an item from a server row
// but keeps a DIFFERENT body must keep that body's `content_state` too, or the
// renders that read the marker label a body it does not describe. Two shapes:
//
// - the local body kept over a fresh row: `{ ...row, content: item.content }`
//   must carry `content_state: item.content_state`;
// - a blanked body built from an index row, which still carries the row's
//   marker: `{ ...row, content: '' }` must clear it, since the server emits the
//   marker only where a real body is.
//
// The scan finds every object literal that spreads something and sets
// `content`, in the files that hold the population (census on the BUG-3050
// trail, round 1). It fails naming the literal.

const SRC = resolve(__dirname, '..');
const FILES = [
	'components/items/ItemDetail.svelte',
	'components/search/CommandPalette.svelte',
	'stores/localIndex.svelte.ts',
	'../routes/[username]/[workspace]/[collection]/+page.svelte',
];

function spreadsSettingContent(source: string): string[] {
	const out: string[] = [];
	const re = /\{\s*\.\.\.[^{}]*?\bcontent\s*:[^{}]*\}/gs;
	for (const m of source.matchAll(re)) out.push(m[0].replace(/\s+/g, ' '));
	return out;
}

function violation(literal: string): string | null {
	const content = /\bcontent\s*:\s*([^,}]+)/.exec(literal)?.[1].trim();
	if (content === undefined) return null;
	// The row's own body travels with its own marker: nothing to carry.
	if (/^\w+\.content$/.test(content) && literal.includes(`...${content.split('.')[0]}`)) return null;
	if (content === "''" || content === '""') {
		return /content_state\s*:\s*undefined/.test(literal) ? null : 'blank body keeps the row\'s marker';
	}
	const owner = /^(\w+)\??\.content$/.exec(content)?.[1];
	if (owner && new RegExp(`content_state\\s*:\\s*${owner}\\??\\.content_state`).test(literal)) return null;
	return `kept body ${content} without its content_state`;
}

describe('a kept or blanked body carries the marker that describes it (BUG-3050 U3)', () => {
	it('the scan sees the population it was written for', () => {
		const all = FILES.flatMap((f) => spreadsSettingContent(readFileSync(resolve(SRC, f), 'utf8')));
		// 5 kept-body sites in ItemDetail + 3 blanked index rows.
		expect(all.length).toBe(8);
	});

	for (const f of FILES) {
		it(`${f}: every such literal carries the right marker`, () => {
			const bad = spreadsSettingContent(readFileSync(resolve(SRC, f), 'utf8'))
				.map((l) => [l, violation(l)] as const)
				.filter(([, v]) => v !== null)
				.map(([l, v]) => `${v}: ${l}`);
			expect(bad).toEqual([]);
		});
	}

	it('the checker refuses each broken shape (negative controls)', () => {
		expect(violation("{ ...updated, content: item.content }")).not.toBeNull();
		expect(violation("{ ...row, content: '' }")).not.toBeNull();
		expect(violation("{ ...updated, content: item.content, content_state: updated.content_state }")).not.toBeNull();
		expect(violation("{ ...updated, content: item.content, content_state: item.content_state }")).toBeNull();
		expect(violation("{ ...row, content: '', content_state: undefined }")).toBeNull();
	});
});
