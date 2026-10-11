import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { execSync } from 'node:child_process';

// BUG-3571 wiring: every sign-out control asks first, and the open item stands
// down its teardown save and unload prompt during a confirmed discard. The
// population is found by search, not listed by hand, so a sixth sign-out
// control without the ask fails here.
describe('BUG-3571: sign-out wiring', () => {
	const sites = execSync("grep -rl --include=*.svelte 'api.auth.logout()' src", { encoding: 'utf8' })
		.trim()
		.split('\n')
		.filter(Boolean);

	it('finds the sign-out controls (vantage point)', () => {
		expect(sites.length).toBeGreaterThanOrEqual(5);
	});

	for (const file of sites) {
		it(`${file} asks confirmSignOut() before api.auth.logout()`, () => {
			const src = readFileSync(file, 'utf8');
			const ask = src.indexOf('await confirmSignOut()');
			const logout = src.indexOf('api.auth.logout()');
			expect(ask, 'asks first').toBeGreaterThan(-1);
			expect(ask).toBeLessThan(logout);
		});
	}

	it('ItemDetail registers the guard and stands down its teardown and unload prompt', () => {
		const src = readFileSync('src/lib/components/items/ItemDetail.svelte', 'utf8');
		expect(src).toContain('registerSignOutGuard({');
		const teardown = src.slice(src.indexOf('function runTeardownFlush(): void {'));
		expect(teardown.slice(0, 400)).toContain('if (signOutDiscarding()) return;');
		const unload = src.slice(src.indexOf('const onBeforeUnload = (event: BeforeUnloadEvent) => {'));
		expect(unload.slice(0, 1600)).toContain('if (signOutDiscarding()) return;');
	});
});
