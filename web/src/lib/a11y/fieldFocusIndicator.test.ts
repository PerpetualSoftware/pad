import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join, relative } from 'node:path';

/**
 * TASK-3461 (after TASK-2262): every keyboard-focusable field shows a visible
 * focus indicator. app.css gives every input, textarea and select a 2px
 * :focus-visible ring; a component rule `X:focus { outline: none }` on a field
 * beats that ring, because a scoped class selector outranks it. 40 such rules
 * were found and removed. This keeps a 41st from arriving unnoticed.
 *
 * A rule is a FIELD rule when its selector names input / textarea / select, or
 * names a class that the same file's markup puts on one of those elements. It
 * may set `outline: none` under :focus only if the same rule gives another
 * visible indicator (a box-shadow), which is the escape hatch for a borderless
 * field whose design wants something other than a ring.
 */

const SRC = fileURLToPath(new URL('../..', import.meta.url));

function svelteFiles(dir: string): string[] {
	const out: string[] = [];
	for (const e of readdirSync(dir, { withFileTypes: true })) {
		const p = join(dir, e.name);
		if (e.isDirectory()) out.push(...svelteFiles(p));
		else if (e.name.endsWith('.svelte')) out.push(p);
	}
	return out;
}

interface Offender {
	file: string;
	line: number;
	selector: string;
}

function fieldFocusSuppressions(file: string, text: string): Offender[] {
	const at = text.indexOf('<style');
	if (at < 0) return [];
	const markup = text.slice(0, at);
	const style = text.slice(at);
	const out: Offender[] = [];
	const rule = /([^{}]*?):focus\b(?!-)([^{}]*)\{([^}]*)\}/g;
	for (const m of style.matchAll(rule)) {
		const body = m[3];
		if (!/outline:\s*none/.test(body)) continue;
		if (/box-shadow:\s*(?!none)/.test(body)) continue; // another indicator
		const selector = (m[1] + ':focus' + m[2]).trim();
		const isField = selector.split(',').some((part) => {
			if (!part.includes(':focus') || part.includes(':focus-visible')) return false;
			const base = part.split(':focus')[0];
			if (/(?:^|\s)(input|textarea|select)\b/.test(base)) return true;
			return [...base.matchAll(/\.([A-Za-z0-9_-]+)/g)].some(([, cls]) =>
				new RegExp(`<(input|textarea|select)\\b[^>]*class(?:=|:)[^>]*\\b${cls}\\b`, 's').test(markup)
			);
		});
		if (isField) {
			const line = text.slice(0, at + (m.index ?? 0)).split('\n').length;
			out.push({ file: relative(SRC, file), line, selector });
		}
	}
	return out;
}

describe('every field keeps a visible focus indicator (TASK-3461)', () => {
	it('no component suppresses the focus ring on a field without another indicator', () => {
		const offenders = svelteFiles(SRC).flatMap((f) => fieldFocusSuppressions(f, readFileSync(f, 'utf8')));
		expect(offenders).toEqual([]);
	});

	it('CONTROL: the detector finds a suppression it is shown', () => {
		const sample = `<input class="name-input" />\n<style>\n\t.name-input:focus { border-color: blue; outline: none; }\n</style>`;
		expect(fieldFocusSuppressions(join(SRC, 'x.svelte'), sample)).toHaveLength(1);
	});

	it('CONTROL: a non-field rule and a rule with a box-shadow indicator are not flagged', () => {
		const sample = `<button class="btn"></button><input class="t" />\n<style>\n\t.btn:focus { outline: none; }\n\t.t:focus { outline: none; box-shadow: 0 0 0 2px blue; }\n</style>`;
		expect(fieldFocusSuppressions(join(SRC, 'x.svelte'), sample)).toEqual([]);
	});
});
