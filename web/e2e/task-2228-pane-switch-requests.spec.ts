import type { APIRequestContext, Page, Request } from '@playwright/test';
import { test, expect, type SuiteFixture } from './fixtures';
import { browserLogin } from './lib/collab-helpers';

/**
 * TASK-2228 (audit C77): switching the split pane from one item to the next
 * (j/k, or a click) paid duplicate round trips: dependent panels fetched once
 * for the ref-derived slug and again when the resolved id landed, the
 * collection schema was refetched for every item, and progress and links ran
 * one after the other. This records the API requests one switch makes.
 */

function authHeaders(fixture: SuiteFixture) {
	return { Authorization: `Bearer ${fixture.apiToken}`, 'Content-Type': 'application/json' };
}

async function seed(fixture: SuiteFixture, request: APIRequestContext) {
	const name = `Switch ${Date.now()}`;
	const schema = JSON.stringify({ fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] }] });
	const c = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections`, {
		headers: authHeaders(fixture),
		data: { name, prefix: 'SW' + String(Date.now()).slice(-4), schema },
	});
	if (!c.ok()) throw new Error(`collection create: ${c.status()} ${await c.text()}`);
	const coll = (await c.json()) as { slug: string };
	for (let i = 0; i < 5; i++) {
		const r = await request.post(`/api/v1/workspaces/${fixture.workspaceSlug}/collections/${coll.slug}/items`, {
			headers: authHeaders(fixture),
			data: { title: `Switch item ${i}`, fields: JSON.stringify({ status: 'open' }), content: `Body ${i}` },
		});
		if (!r.ok()) throw new Error(`item create: ${r.status()} ${await r.text()}`);
	}
	return coll.slug;
}

/** API requests the page makes while `act` runs and the pane settles. */
async function apiRequestsDuring(page: Page, act: () => Promise<void>): Promise<string[]> {
	const seen: string[] = [];
	const onReq = (r: Request) => {
		const u = new URL(r.url());
		if (u.pathname.startsWith('/api/v1/') && !u.pathname.startsWith('/api/v1/events')) seen.push(`${r.method()} ${u.pathname}${u.search}`);
	};
	page.on('request', onReq);
	await act();
	await page.waitForLoadState('networkidle');
	await page.waitForTimeout(400);
	page.off('request', onReq);
	return seen;
}

test('a split-pane switch makes each request once', async ({ page, fixture, request }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop-chromium', 'the split pane is a desktop layout');
	test.setTimeout(60_000);
	const slug = await seed(fixture, request);
	await browserLogin(page);
	await page.goto(`/${fixture.adminUsername}/${fixture.workspaceSlug}/${slug}?view=list`);
	const first = page.locator('.item-card').first();
	await expect(first).toBeVisible();
	await first.click();
	await expect(page.locator('.item-pane')).toBeVisible();
	await page.waitForLoadState('networkidle');

	const perSwitch: string[][] = [];
	for (let i = 0; i < 4; i++) {
		perSwitch.push(await apiRequestsDuring(page, async () => { await page.keyboard.press('j'); }));
	}
	for (const [i, reqs] of perSwitch.entries()) {
		testInfo.annotations.push({ type: `switch-${i + 1}`, description: `${reqs.length}: ${reqs.join(' | ')}` });
		console.log(`switch ${i + 1}: ${reqs.length} requests`);
		for (const r of reqs) console.log('   ' + r);
	}
	for (const reqs of perSwitch) {
		const dupes = reqs.filter((r, i) => reqs.indexOf(r) !== i);
		expect(dupes, 'a request made twice in one switch').toEqual([]);
	}
});
