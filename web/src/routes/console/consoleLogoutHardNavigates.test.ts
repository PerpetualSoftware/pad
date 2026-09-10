// Node-project test (no DOM): a SOURCE guard that the console logout leaves the
// page by a HARD navigation (BUG-3005).
//
// `goto('/login')` is an SPA navigation. It leaves every client-side singleton,
// component and cache exactly where it was — which is this bug's original
// defect: the next person to sign in on this browser reads the previous user's
// data out of a tab that never went away. Account deletion in
// `console/settings/+page.svelte` has always used `window.location.href` and
// says why in its own comment; this makes the two sign-out sites agree.
//
// It is a source guard because the alternative is rendering the console layout
// to observe an assignment to `window.location`, which jsdom does not perform.
// WHAT A SOURCE GUARD CANNOT DO: it checks spellings, not behaviour — a logout
// that hard-navigates somewhere useless would still pass.
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

function logoutBody(file: string): string {
	const src = readFileSync(resolve(__dirname, file), 'utf8').replace(/^[ \t]*\/\/.*$/gm, '');
	const start = src.indexOf('async function logout()');
	expect(start, `${file} no longer declares logout()`).toBeGreaterThan(-1);
	return src.slice(start, src.indexOf('\n\t}', start));
}

describe('console sign-out', () => {
	it('hard-navigates rather than using goto', () => {
		const body = logoutBody('./+layout.svelte');
		expect(body).toContain("window.location.href = '/login'");
		expect(body).not.toContain('goto(');
	});

	it('still clears the auth store first', () => {
		// Order matters: `clear()` is what fires the identity listeners that run
		// the persistent clears, and a navigation started first can cut them off.
		const body = logoutBody('./+layout.svelte');
		expect(body.indexOf('authStore.clear()')).toBeLessThan(body.indexOf('window.location.href'));
	});

	it('account deletion still hard-navigates too — the site this one now matches', () => {
		const src = readFileSync(resolve(__dirname, './settings/+page.svelte'), 'utf8');
		expect(src).toContain("window.location.href = '/login'");
	});
});
