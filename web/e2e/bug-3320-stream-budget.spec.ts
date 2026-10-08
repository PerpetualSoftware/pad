import { test, expect } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * BUG-3320, the measured repro going green. The e2e server is plain HTTP, so
 * the browser speaks HTTP/1.1 and allows 6 connections per host. Measured
 * before the fix: with the access stream plus one stream per workspace
 * leader, the 5th workspace tab's first fetch never left the browser and a
 * 6th tab could not load its HTML.
 *
 * Now a workspace leader on HTTP/1.1 needs one of 3 browser-wide stream
 * slots, and the others poll. So six workspaces in six tabs of ONE browser
 * profile: every tab loads, the first three are Live, the rest say
 * "Live (polling)". h2/h3 take no slot; that branch is unit-tested
 * (sseStreamBudget.svelte.test.ts), since this server has no TLS.
 */
test('six workspaces in six tabs all load on HTTP/1.1; tabs past the budget poll', async ({ page, context, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the connection pool is per browser, not per viewport');
	test.setTimeout(120_000);

	const headers = { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
	const slugs: string[] = [];
	for (let i = 1; i <= 6; i++) {
		const resp = await request.post('/api/v1/workspaces', {
			headers,
			data: { name: `Budget ${i} ${Date.now()}`, template: 'startup' }
		});
		expect(resp.ok(), await resp.text()).toBe(true);
		slugs.push(((await resp.json()) as { slug: string }).slug);
	}

	await browserLogin(page);
	const protocol = await page.evaluate(
		() => (performance.getEntriesByType('navigation')[0] as PerformanceNavigationTiming)?.nextHopProtocol
	);
	expect(protocol, 'premise: this server speaks HTTP/1.1').toBe('http/1.1');

	const tabs = [];
	for (const [i, slug] of slugs.entries()) {
		const tab = i === 0 ? page : await context.newPage();
		await tab.goto(`/${fixture.adminUsername}/${slug}/tasks`);
		// Before the fix the 5th tab sat on a skeleton and the 6th never loaded.
		await expect(tab.locator('.sse-state').first(), `tab ${i + 1} (${slug}) loads`).toBeVisible({ timeout: 20_000 });
		await expect(tab.locator('.sse-state-connected, .sse-state-polling').first()).toBeVisible({ timeout: 20_000 });
		tabs.push(tab);
	}

	const states = [];
	for (const tab of tabs) {
		states.push((await tab.locator('.sse-state').first().getAttribute('class')) ?? '');
	}
	const live = states.filter((c) => c.includes('sse-state-connected')).length;
	const polling = states.filter((c) => c.includes('sse-state-polling')).length;
	expect(live, states.join(' | ')).toBe(3);
	expect(polling, states.join(' | ')).toBe(3);
	await expect(tabs[5].locator('.sse-state-polling').first()).toContainText('Live (polling)');
});
