import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join, relative } from 'node:path';

/**
 * BUG-3465 (WCAG 2.1.4): every single-character keyboard shortcut must be
 * governed by the single-key switch, which means it reads its key through
 * `characterKey()` (lib/a11y/characterShortcuts.svelte.ts) instead of
 * comparing `event.key` / `event.code` against a printable character itself.
 *
 * This census fails on any raw single-character key comparison in web/src,
 * so a new binding cannot skip the switch. The exceptions are bindings that
 * 2.1.4 itself exempts, "active only when that component has focus", each
 * listed with its reason.
 *
 * Two kinds of comparison are not single-character shortcuts and are not
 * flagged:
 * - SPACE: Enter/Space activation of the focused control is how a button
 *   works, not a shortcut (about 15 sites), and characterKey() never answers
 *   Space.
 * - A modifier CHORD on the same line (`isMod(e) && e.key === 'k'`,
 *   `e.ctrlKey && …`): 2.1.4 covers character-only shortcuts. A NEGATED
 *   modifier (`&& !e.ctrlKey`) is still flagged, since it means "no modifier".
 *
 * Scope, stated rather than implied: it matches the spellings below. A key
 * copied into a variable first (`const k = e.key; if (k === 'x')`) is not
 * seen; route new bindings through characterKey() and that case never arises.
 */

const SRC = fileURLToPath(new URL('../..', import.meta.url));
const HELPER = 'lib/a11y/characterShortcuts.svelte.ts';

/** file::character → why the binding is exempt from the switch. */
const FOCUS_SCOPED: Record<string, string> = {
	'lib/components/editor/Editor.svelte::[':
		'typing [[ inside the focused editor opens the link picker: text input, active only on focus',
	'lib/components/editor/Editor.svelte::/':
		'typing / inside the focused editor opens the slash menu: text input, active only on focus',
	'lib/components/fields/TagInput.svelte::,':
		'a comma typed in the focused tag field commits the tag: text input, active only on focus'
};

/** `isMod(e) &&`, `e.ctrlKey &&`, `event.metaKey ||` …, but not `!e.ctrlKey`. */
const CHORD = /(?<!!)\b(isMod\(\w+\)|\w+\.(ctrlKey|metaKey|altKey))\s*(&&|\|\|)/;

const CODE_KEYS =
	'Key[A-Z]|Digit[0-9]|Slash|Period|Comma|Minus|Equal|BracketLeft|BracketRight|Backslash|Semicolon|Quote|Backquote';

interface Hit {
	file: string;
	line: number;
	char: string;
	text: string;
}

function sourceFiles(dir: string): string[] {
	const out: string[] = [];
	for (const e of readdirSync(dir, { withFileTypes: true })) {
		const p = join(dir, e.name);
		if (e.isDirectory()) out.push(...sourceFiles(p));
		else if (/\.(svelte|ts)$/.test(e.name) && !/\.(test|spec)\./.test(e.name)) out.push(p);
	}
	return out;
}

function characterKeyComparisons(file: string, text: string): Hit[] {
	const hits: Hit[] = [];
	const handlesKeys = /keydown|keyup|keypress|KeyboardEvent/.test(text);
	const patterns: RegExp[] = [
		// e.key === 'x', event.key !== "x"
		/\.key\s*[!=]==?\s*(['"`])(.)\1/g,
		// 'x' === e.key
		/(['"`])(.)\1\s*[!=]==?\s*[\w.]*\.key\b/g,
		// e.code === 'KeyX' / 'Digit1' / 'Slash' …; the character is the code name
		new RegExp(`\\.code\\s*[!=]==?\\s*(['"\`])(${CODE_KEYS})\\1`, 'g'),
		// ['j', 'k'].includes(e.key): a list match, flagged whatever it holds
		/\.includes\(\s*[\w.]*\.key\s*\)()(.)?/g
	];
	if (handlesKeys) patterns.push(/case\s+(['"`])(.)\1\s*:/g); // switch (e.key) { case 'j': }
	const lines = text.split('\n');
	for (const re of patterns) {
		for (const m of text.matchAll(re)) {
			const line = text.slice(0, m.index).split('\n').length;
			if (m[2] === ' ' || CHORD.test(lines[line - 1])) continue;
			hits.push({
				file: relative(SRC, file),
				line,
				char: m[2] ?? '',
				text: text.split('\n')[line - 1].trim()
			});
		}
	}
	return hits;
}

describe('every single-character shortcut is governed by the single-key switch (BUG-3465)', () => {
	const hits = sourceFiles(SRC)
		.filter((f) => relative(SRC, f) !== HELPER)
		.flatMap((f) => characterKeyComparisons(f, readFileSync(f, 'utf8')));

	it('no raw single-character key comparison outside characterKey() and the focus-scoped list', () => {
		const ungoverned = hits.filter((h) => !(`${h.file}::${h.char}` in FOCUS_SCOPED));
		expect(
			ungoverned.map((h) => `${h.file}:${h.line}  ${h.text}`),
			'read the key through characterKey() so the single-key switch turns it off, or, if it ' +
				'is active only while its own component has focus, add it to FOCUS_SCOPED with the reason'
		).toEqual([]);
	});

	it('every FOCUS_SCOPED entry is still a live binding (no stale exemptions)', () => {
		const live = new Set(hits.map((h) => `${h.file}::${h.char}`));
		expect(Object.keys(FOCUS_SCOPED).filter((k) => !live.has(k))).toEqual([]);
	});

	it('CONTROL: each spelling is detected when planted', () => {
		const planted = [
			"if (e.key === 'c') {}",
			"if ('c' === event.key) {}",
			"if (e.code === 'KeyC') {}",
			"if (['j', 'k'].includes(e.key)) {}",
			"function onKeydown(e: KeyboardEvent) { switch (e.key) { case 'c': break; } }",
			// a NEGATED modifier is "no modifier held": still a character shortcut
			"if (e.key === 'c' && !e.ctrlKey && !e.metaKey) {}"
		];
		for (const sample of planted) {
			expect({ sample, found: characterKeyComparisons(join(SRC, 'x.ts'), sample).length > 0 }).toEqual({
				sample,
				found: true
			});
		}
	});

	it('CONTROL: governed and non-character comparisons are not flagged', () => {
		const clean = [
			"const ch = characterKey(e); if (ch === 'c') {}",
			"if (e.key === 'Escape' || e.key === 'Enter' || e.key === 'ArrowDown') {}",
			"switch (status) { case 'a': break; }", // a switch in a file that handles no keys
			"if (e.key === 'Enter' || e.key === ' ') activate();", // Space activation
			"if (isMod(e) && e.key === 'k') {}", // a modifier chord
			"if (e.ctrlKey && e.key === 'j') {}"
		];
		for (const sample of clean) {
			expect({ sample, hits: characterKeyComparisons(join(SRC, 'x.ts'), sample) }).toEqual({
				sample,
				hits: []
			});
		}
	});
});
