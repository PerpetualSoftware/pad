// TASK-2236 (audit C52): about 50 selects announced as an indistinguishable
// "combo box". Every <select> in web/src now carries a name: aria-label,
// aria-labelledby, an id a <label for> names, or a wrapping <label>.
// TASK-3515 extends the floor to text inputs and textareas that carry a
// placeholder: a placeholder is not a name (screen readers do not reliably
// announce it, and it disappears once the field has text), so such a control
// needs one of the same four. Other controls' names are checked by behaviour
// tests where they matter.
import { describe, it, expect } from 'vitest';
import fs from 'node:fs';
import path from 'node:path';

const SRC = path.resolve(__dirname, '../..');

function svelteFiles(dir: string): string[] {
	return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
		const p = path.join(dir, e.name);
		if (e.isDirectory()) return svelteFiles(p);
		return p.endsWith('.svelte') ? [p] : [];
	});
}

// Comments mention "<select>" in prose; mask them, keeping line numbers.
const mask = (t: string) => t.replace(/[^\n]/g, ' ');
function code(text: string): string {
	return text
		.replace(/<!--[\s\S]*?-->/g, mask)
		.replace(/\/\*[\s\S]*?\*\//g, mask)
		.replace(/^[ \t]*\/\/.*$/gm, mask);
}

// The end of a tag that opens at `i`: the first `>` outside braces and
// quotes. A plain `[^>]*` stops inside `oninput={(e) => …}`, which inputs carry
// far more often than selects (TASK-3515).
function tagEnd(s: string, i: number): number {
	let depth = 0;
	let quote: string | null = null;
	for (; i < s.length; i++) {
		const c = s[i];
		if (quote) {
			if (c === quote) quote = null;
			continue;
		}
		if (c === '"' || (depth > 0 && (c === "'" || c === '`'))) quote = c;
		else if (c === '{') depth++;
		else if (c === '}') depth--;
		else if (c === '>' && depth === 0) return i;
	}
	return s.length;
}

// Controls matching `open` (a tag-opening regex) that `needsName` selects and
// that have no name.
function unnamed(file: string, open: RegExp, needsName: (attrs: string) => boolean): string[] {
	const s = code(fs.readFileSync(file, 'utf8'));
	const labelFor = new Set(
		[...s.matchAll(/<label[^>]*\sfor=(?:"([^"]+)"|\{([^}]+)\})/g)].map((m) => m[1] ?? `{${m[2]}}`)
	);
	const out: string[] = [];
	for (const m of s.matchAll(open)) {
		const attrs = s.slice(m.index!, tagEnd(s, m.index!));
		if (!needsName(attrs)) continue;
		if (/aria-label(ledby)?=/.test(attrs)) continue;
		const idm = attrs.match(/\sid=(?:"([^"]+)"|\{([^}]+)\})/);
		const id = idm ? (idm[1] ?? `{${idm[2]}}`) : undefined;
		if (id && labelFor.has(id)) continue;
		const before = s.slice(0, m.index);
		if (before.lastIndexOf('<label') > before.lastIndexOf('</label>')) continue;
		out.push(`${path.relative(SRC, file)}:${before.split('\n').length}`);
	}
	return out;
}

const unnamedSelects = (file: string) => unnamed(file, /<select\b/g, () => true);
const unnamedPlaceholderFields = (file: string) =>
	unnamed(
		file,
		/<(input|textarea)\b/g,
		(a) => /\splaceholder=/.test(a) && !/\stype="(hidden|checkbox|radio|file)"/.test(a)
	);
// A file input still in the accessibility tree (visually hidden, not
// aria-hidden or display:none) is a focus stop with no name: axe found two on
// the Conventions and Playbooks pages (TASK-3515).
const unnamedFileInputs = (file: string) =>
	unnamed(
		file,
		/<input\b/g,
		(a) => /\stype="file"/.test(a) && !/aria-hidden="true"/.test(a) && !/display:\s*none/.test(a)
	);

describe('TASK-2236: every <select> has an accessible name', () => {
	it('finds no unnamed select', () => {
		const offenders = svelteFiles(SRC).flatMap(unnamedSelects);
		expect(offenders, 'give each a name: aria-label, aria-labelledby, or a <label for>').toEqual([]);
	});

	it('CONTROL: the scan sees an unnamed select, and accepts each way of naming one', () => {
		const tmp = path.join(SRC, '..', '.task2236-probe.svelte');
		fs.writeFileSync(
			tmp,
			[
				'<!-- a <select> in a comment is not one -->',
				'<select bind:value={a}></select>',
				'<select aria-label="Named"></select>',
				'<label for="x">X</label><select id="x"></select>',
				'<label>Wrapped <select></select></label>'
			].join('\n')
		);
		try {
			expect(unnamedSelects(tmp).map((o) => o.split(':').pop())).toEqual(['2']);
		} finally {
			fs.unlinkSync(tmp);
		}
	});
});

describe('TASK-3515: every text field with a placeholder has an accessible name', () => {
	it('finds no field named only by its placeholder', () => {
		const offenders = svelteFiles(SRC).flatMap(unnamedPlaceholderFields);
		expect(offenders, 'a placeholder is not a name: add aria-label, aria-labelledby, or a <label for>').toEqual([]);
	});

	it('finds no file input in the accessibility tree without a name', () => {
		const offenders = svelteFiles(SRC).flatMap(unnamedFileInputs);
		expect(offenders, 'name it, or take it out of the tree (aria-hidden + tabindex=-1, or display:none)').toEqual([]);
	});

	it('CONTROL: the file scan sees a reachable unnamed file input and accepts the hidden ones', () => {
		const tmp = path.join(SRC, '..', '.task3515-file-probe.svelte');
		fs.writeFileSync(
			tmp,
			[
				'<input type="file" class="visually-hidden-input" />',
				'<input type="file" aria-label="Import" />',
				'<input type="file" aria-hidden="true" tabindex="-1" />',
				'<input type="file" style="display:none" />'
			].join('\n')
		);
		try {
			expect(unnamedFileInputs(tmp).map((o) => o.split(':').pop())).toEqual(['1']);
		} finally {
			fs.unlinkSync(tmp);
		}
	});

	it('CONTROL: the scan sees a placeholder-only field past an arrow handler, and accepts each way of naming one', () => {
		const tmp = path.join(SRC, '..', '.task3515-probe.svelte');
		fs.writeFileSync(
			tmp,
			[
				'<!-- <input placeholder="in a comment"> is not one -->',
				'<input oninput={(e) => (x = e.target.value)} placeholder="Unnamed" />',
				'<textarea placeholder="Unnamed too"></textarea>',
				'<input aria-label="Named" placeholder="p" />',
				'<label for="y">Y</label><input id="y" placeholder="p" />',
				'<label>Wrapped <input placeholder="p" /></label>',
				'<input type="checkbox" placeholder="not a text field" />',
				'<input value="no placeholder" />'
			].join('\n')
		);
		try {
			expect(unnamedPlaceholderFields(tmp).map((o) => o.split(':').pop())).toEqual(['2', '3']);
		} finally {
			fs.unlinkSync(tmp);
		}
	});
});
