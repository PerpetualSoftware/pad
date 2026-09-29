import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join, relative } from 'node:path';

/**
 * TASK-3299 (PLAN-3291 DR-1): every getpad.dev address the web app can render
 * is CLASSIFIED, so that a new one fails here until someone decides whether it
 * belongs in the mobile apps.
 *
 * KEPT addresses are legal and docs pages (the lead's ruling, day 82): the
 * privacy policy must be reachable in the app, and none of them sells
 * anything. GATED addresses are the marketing site (its root, blog, FAQ,
 * contribute) and the Pro-interest mailto, and each file that names one must
 * read the shell gate. What actually renders in the app is checked by the
 * shell-UA e2e; this is the population check.
 */

const SRC = fileURLToPath(new URL('../..', import.meta.url));

const KEPT: RegExp[] = [
	/^https:\/\/getpad\.dev\/docs(\/|#|$)/,
	/^https:\/\/getpad\.dev\/(changelog|privacy|terms|subprocessors|security|connect)$/,
	/^https:\/\/status\.getpad\.dev$/,
	/^mailto:support@getpad\.dev$/
];

const GATED: RegExp[] = [
	/^https:\/\/getpad\.dev\/?$/,
	/^https:\/\/getpad\.dev\/(blog|faq|contribute)$/,
	/^mailto:info@getpad\.dev(\?|$)/
];

const ADDRESS = /(?:https?:\/\/(?:[a-z0-9-]+\.)*getpad\.dev[^\s'"`<>)]*|mailto:[a-z0-9._-]+@getpad\.dev[^\s'"`<>)]*)/gi;

function sources(dir: string): string[] {
	const out: string[] = [];
	for (const e of readdirSync(dir, { withFileTypes: true })) {
		const p = join(dir, e.name);
		if (e.isDirectory()) out.push(...sources(p));
		else if (/\.(svelte|ts)$/.test(e.name) && !/\.(test|spec)\.ts$/.test(e.name)) out.push(p);
	}
	return out;
}

// The gate must be in CODE: a comment naming it is not a gate (a mutant that
// removed the share page's gate survived on its own explanatory comment).
function codeOnly(text: string): string {
	return text
		.replace(/<!--[\s\S]*?-->/g, '')
		.replace(/\/\*[\s\S]*?\*\//g, '')
		.split('\n')
		.filter((l) => !l.trim().startsWith('//'))
		.join('\n');
}

function isComment(line: string): boolean {
	const t = line.trim();
	return t.startsWith('//') || t.startsWith('*') || t.startsWith('/*') || t.startsWith('<!--');
}

describe('getpad.dev addresses are classified', () => {
	const found: { rel: string; line: number; url: string; text: string }[] = [];
	for (const p of sources(SRC)) {
		const text = readFileSync(p, 'utf8');
		text.split('\n').forEach((l, i) => {
			if (isComment(l)) return;
			for (const m of l.matchAll(ADDRESS)) found.push({ rel: relative(SRC, p), line: i + 1, url: m[0], text });
		});
	}

	it('finds the known population (vacuity guard)', () => {
		expect(found.length).toBeGreaterThan(15);
		expect(found.some((f) => GATED[0].test(f.url))).toBe(true);
	});

	it('every address is kept or gated', () => {
		const unclassified = found
			.filter((f) => !KEPT.some((r) => r.test(f.url)) && !GATED.some((r) => r.test(f.url)))
			.map((f) => `${f.rel}:${f.line} ${f.url}`);
		expect(unclassified).toEqual([]);
	});

	it('every file naming a gated address reads the shell gate', () => {
		const ungated = found
			.filter((f) => GATED.some((r) => r.test(f.url)))
			.filter((f) => !/nativeShell|commerceAllowed/.test(codeOnly(f.text)))
			.map((f) => `${f.rel}:${f.line} ${f.url}`);
		expect(ungated).toEqual([]);
	});
});
