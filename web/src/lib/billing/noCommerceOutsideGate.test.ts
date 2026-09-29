import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { join, relative } from 'node:path';

/**
 * PLAN-3291 DR-3 acceptance (TASK-3293): no upgrade copy and no billing
 * destination anywhere in the web app outside showPlanLimitToast and the
 * components that gate on authStore.commerceAllowed. The mobile apps render
 * this web app, so an ungated one is a purchase call to action inside them
 * (BUG-3290).
 *
 * This is one layer of four, and the weakest: it defines the population by
 * text, so wording it does not match, or a URL built at runtime, slips past.
 * The shell-UA e2e (e2e/task-3293-no-commerce-in-app.spec.ts) checks what
 * actually renders; the server and the shells guard the rest (DR-4, DR-5).
 *
 * A gated file is exempt WHOLE: the scan proves only that it reads
 * commerceAllowed, not that every surface in it sits under that gate. Line
 * scoping would need a template parser, and a hand one is its own review
 * loop. What renders inside those files is the e2e's job; it visits settings,
 * billing (bare and both checkout returns) and the toast with both UAs.
 */

const SRC = fileURLToPath(new URL('../..', import.meta.url));

// Each of these reads commerceAllowed; the test below checks that it does.
const GATED = new Set([
	'lib/billing/planLimitToast.ts',
	'lib/components/layout/TopBar.svelte',
	'lib/components/layout/YouSheet.svelte',
	'routes/console/+layout.svelte',
	'routes/console/settings/+page.svelte',
	'routes/console/billing/+page.svelte'
]);

const PATTERNS: [string, RegExp][] = [
	['upgrade copy', /Upgrade to Pro/],
	['billing page link', /["'`]\/console\/billing["'`?#]/],
	['billing portal link', /\/billing\/portal/],
	['view plans copy', /View Plans/],
	['manage billing copy', /Manage Billing/]
];

function sources(dir: string): string[] {
	const out: string[] = [];
	for (const e of readdirSync(dir, { withFileTypes: true })) {
		const p = join(dir, e.name);
		if (e.isDirectory()) out.push(...sources(p));
		else if (/\.(svelte|ts)$/.test(e.name) && !/\.(test|spec)\.ts$/.test(e.name)) out.push(p);
	}
	return out;
}

// A comment may name the page; only code can render it.
function isComment(line: string): boolean {
	const t = line.trim();
	return t.startsWith('//') || t.startsWith('*') || t.startsWith('/*') || t.startsWith('<!--');
}

describe('no commerce outside the commerceAllowed gate', () => {
	const files = sources(SRC).map((p) => ({ rel: relative(SRC, p), text: readFileSync(p, 'utf8') }));

	it('scans a real tree (vacuity guard)', () => {
		expect(files.length).toBeGreaterThan(200);
		for (const g of GATED) expect(files.some((f) => f.rel === g), g).toBe(true);
	});

	it('finds upgrade copy and billing links only in gated files', () => {
		const hits: string[] = [];
		for (const f of files) {
			if (GATED.has(f.rel)) continue;
			f.text.split('\n').forEach((line, i) => {
				if (isComment(line)) return;
				for (const [what, re] of PATTERNS) {
					if (re.test(line)) hits.push(`${f.rel}:${i + 1} ${what}: ${line.trim()}`);
				}
			});
		}
		expect(hits).toEqual([]);
	});

	it('every gated file reads commerceAllowed', () => {
		for (const f of files) {
			if (GATED.has(f.rel)) expect(f.text, f.rel).toMatch(/commerceAllowed/);
		}
	});
});
