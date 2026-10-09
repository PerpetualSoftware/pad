// TASK-2236 (audit C52): about 50 selects announced as an indistinguishable
// "combo box". Every <select> in web/src now carries a name: aria-label,
// aria-labelledby, an id a <label for> names, or a wrapping <label>. This is a
// FLOOR for selects only, the narrow, stable case; other controls' names are
// checked by behaviour tests where they matter.
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

function unnamedSelects(file: string): string[] {
	const s = code(fs.readFileSync(file, 'utf8'));
	const labelFor = new Set(
		[...s.matchAll(/<label[^>]*\sfor=(?:"([^"]+)"|\{([^}]+)\})/g)].map((m) => m[1] ?? `{${m[2]}}`)
	);
	const out: string[] = [];
	for (const m of s.matchAll(/<select\b([^>]*?)>/gs)) {
		const attrs = m[1];
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
