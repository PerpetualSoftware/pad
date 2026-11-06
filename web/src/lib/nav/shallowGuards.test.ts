// TASK-4315: a SHALLOW navigation (`goto(url, { shallow: true })`, which Chat
// uses to open over whatever page the user is on) fires every `beforeNavigate`
// and `afterNavigate` callback in the app, with `nav.shallow === true` and a
// `nav.to` that is the Chat target. The page underneath does not change, so a
// callback that guards or latches on "the user is leaving" would prompt, cancel
// or replay for a navigation that leaves nothing.
//
// This census regenerates the registration list from the source, so a callback
// added later cannot silently skip the question. Every registration must read
// `.shallow` in its callback, or be allow-listed below with the reason a shallow
// navigation is correct for it to see.
//
// Behavioural halves live next to this file: shallowGuards.*.svelte.test.ts.
import { describe, expect, it } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';

const SRC = resolve(__dirname, '../..');

/** Registrations that deliberately act on a shallow navigation: path -> [occurrence index -> reason]. */
const ALLOW: Record<string, string> = {
	// (empty: every registration in the app today returns early on shallow.)
};

function walk(dir: string, out: string[] = []): string[] {
	for (const name of readdirSync(dir)) {
		const p = join(dir, name);
		if (statSync(p).isDirectory()) {
			// test/ holds the mocks that DEFINE these functions, not callers.
			if (p === join(SRC, 'test')) continue;
			walk(p, out);
		} else if (/\.(svelte|ts)$/.test(name) && !/\.test\.ts$/.test(name)) {
			out.push(p);
		}
	}
	return out;
}

/** Blank comments out, keeping offsets (so line numbers stay true). */
function blankComments(text: string): string {
	const keep = (m: string) => m.replace(/[^\n]/g, ' ');
	return text
		.replace(/\/\*[\s\S]*?\*\//g, keep)
		.replace(/<!--[\s\S]*?-->/g, keep)
		.replace(/(^|[^:'"`\\])\/\/[^\n]*/g, (m, lead: string) => lead + keep(m.slice(lead.length)));
}

/** The text of the balanced `( ... )` that opens at `open`. */
function balanced(text: string, open: number): string {
	let depth = 0;
	for (let i = open; i < text.length; i++) {
		if (text[i] === '(') depth++;
		else if (text[i] === ')' && --depth === 0) return text.slice(open, i + 1);
	}
	throw new Error('unbalanced registration');
}

interface Registration {
	file: string;
	line: number;
	kind: string;
	body: string;
}

function registrations(): Registration[] {
	const found: Registration[] = [];
	for (const file of walk(SRC)) {
		const raw = readFileSync(file, 'utf8');
		const code = blankComments(raw);
		const re = /\b(beforeNavigate|afterNavigate)\s*\(/g;
		let m: RegExpExecArray | null;
		while ((m = re.exec(code))) {
			// `export function afterNavigate(` style definitions are not registrations.
			if (/function\s+$/.test(code.slice(0, m.index))) continue;
			const open = m.index + m[0].length - 1;
			found.push({
				file: relative(SRC, file),
				line: code.slice(0, m.index).split('\n').length,
				kind: m[1]!,
				body: balanced(code, open)
			});
		}
	}
	return found;
}

describe('TASK-4315: every navigation callback answers for a shallow navigation', () => {
	const all = registrations();

	it('finds the registrations (a rotted regex must not pass vacuously)', () => {
		// 13 at the time of writing; the floor catches the scan going blind.
		expect(all.length).toBeGreaterThanOrEqual(13);
		const files = new Set(all.map((r) => r.file));
		for (const f of [
			'lib/components/items/ItemDetail.svelte',
			'lib/collections/paneHostController.ts',
			'lib/components/layout/TopBar.svelte'
		]) {
			expect(files, `${f} is no longer scanned`).toContain(f);
		}
	});

	it('every beforeNavigate/afterNavigate callback reads .shallow, or is allow-listed with a reason', () => {
		const offenders = all
			.filter((r) => !/\.shallow\b/.test(r.body))
			.filter((r) => !ALLOW[`${r.file}:${r.line}`])
			.map((r) => `${r.file}:${r.line} ${r.kind}`);
		expect(offenders, 'a shallow navigation reaches these callbacks unanswered').toEqual([]);
	});

	it('the allow-list has no stale or reasonless entries', () => {
		const live = new Set(all.map((r) => `${r.file}:${r.line}`));
		for (const [key, reason] of Object.entries(ALLOW)) {
			expect(live.has(key), `${key} no longer exists`).toBe(true);
			expect(reason.trim().length, `${key} needs a reason`).toBeGreaterThan(0);
		}
	});
});
