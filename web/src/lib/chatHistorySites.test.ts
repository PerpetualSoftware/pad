// TASK-4315 PR 3b: the census of every site that writes or traverses browser
// history, or reads the address bar, in the shipped web source. An open Chat is a
// shallow navigation, so a page-owned write at any of these can rewrite Chat's
// own history entry or read Chat's address instead of the page's. A new site
// FAILS here until it is classified in test/chatHistorySites.classified.ts (not
// a problem, with why) or test/chatHistorySites.treated.ts (handled, and the
// file must still carry the call that handles it).
//
// Matching is by trimmed line text and per-file count, not line number, so an
// unrelated edit above a site does not disturb it. Full-line comments and
// imports are skipped mechanically.
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { scanSites, SITE_RX, type Site, type SiteKind } from '../test/chatHistoryScan';
import { CLASSIFIED } from '../test/chatHistorySites.classified';
import { TREATED } from '../test/chatHistorySites.treated';

const SRC = fileURLToPath(new URL('..', import.meta.url));
const SITES: Site[] = [...CLASSIFIED, ...TREATED];
const found = scanSites(SRC);
const key = (file: string, text: string) => `${file}\u0000${text}`;

const counted = new Map<string, number>();
for (const f of found) counted.set(key(f.file, f.text), (counted.get(key(f.file, f.text)) ?? 0) + 1);
const claimed = new Map<string, number>();
for (const s of SITES) claimed.set(key(s.file, s.text), (claimed.get(key(s.file, s.text)) ?? 0) + s.n);

/** What the file must still contain for a row that claims a treatment. */
const NEEDS: Partial<Record<SiteKind, RegExp>> = {
	deferred: /chatSurface\.deferPageWrite\(/,
	'close-then': /chatSurface\.closeThen\(/,
	'after-close': /chatSurface\.runAfterClose\(/,
	'page-location': /\b(currentHref|currentSearch|currentPathSearch|setReturnPathProvider)\b/
};

describe('the scanner', () => {
	it('sees code and skips full-line comments and imports', () => {
		expect(SITE_RX.test('void goto(url, { replaceState: true });')).toBe(true);
		expect(SITE_RX.test('history.go(-1);')).toBe(true);
		expect(SITE_RX.test('const o = window.location.origin;')).toBe(true);
		expect(SITE_RX.test('const fine = page.url.search;')).toBe(false);
		// The real tree has a healthy population, not an empty scan that would pass anything.
		expect(found.length).toBeGreaterThan(100);
	});
});

describe('every history or address-bar site is classified (TASK-4315)', () => {
	it('no site is unclassified', () => {
		const lines = found.filter((f) => !claimed.has(key(f.file, f.text))).map((f) => `${f.file}:${f.line}  ${f.text}`);
		expect(
			lines,
			'These sites write or traverse history, or read the address bar. Under an open Chat that is a shallow ' +
				'navigation: decide whether the site can corrupt Chat\'s entries, then add a row to ' +
				'test/chatHistorySites.classified.ts (not a problem, with why) or .treated.ts (handled).'
		).toEqual([]);
	});

	it('every row still matches a site, and the counts agree', () => {
		const bad: string[] = [];
		for (const [k, n] of claimed) {
			const have = counted.get(k) ?? 0;
			if (have !== n) bad.push(`${k.replace('\u0000', '  ')}: rows claim ${n}, source has ${have}`);
		}
		expect(bad, 'a row is stale (the line changed or moved) or a site was added next to an identical one').toEqual([]);
	});

	it('each row says why in one real line', () => {
		const bad = SITES.filter((s) => s.why.includes('\n') || s.why.trim().length < 20).map((s) => `${s.file}  ${s.text}`);
		expect(bad).toEqual([]);
	});

	it('a handled row names a file that still carries the call that handles it', () => {
		const missing: string[] = [];
		for (const s of SITES) {
			const need = NEEDS[s.kind];
			if (need && !need.test(readFileSync(SRC + s.file, 'utf8'))) missing.push(`${s.file} (${s.kind})`);
		}
		expect([...new Set(missing)], 'the treatment was removed but the row still claims it').toEqual([]);
	});

	it('the shapes that bit before are all handled, not waved through', () => {
		// The files of the ruling's groups that still carry a matching site. (PlanLimitPrompt's read is now a
		// helper call, which the scan no longer matches, so it has no row; its test pins the behaviour.)
		const handled = SITES.filter((s) => ['deferred', 'close-then', 'after-close', 'page-location'].includes(s.kind));
		const files = new Set(handled.map((s) => s.file));
		for (const f of [
			'routes/[username]/[workspace]/[collection]/+page.svelte',
			'routes/[username]/[workspace]/[collection]/[slug]/+page.svelte',
			'lib/components/items/ItemDetail.svelte',
			'lib/collections/paneHostController.ts',
			'routes/[username]/[workspace]/library/+page.svelte',
			'routes/[username]/[workspace]/activity/+page.svelte',
			'routes/[username]/[workspace]/settings/+page.svelte',
			'routes/console/admin/+page.svelte',
			'routes/console/billing/+page.svelte',
			'lib/components/admin/UserModal.svelte',
			'lib/api/client.ts'
		]) {
			expect(files.has(f), f).toBe(true);
		}
	});
});
